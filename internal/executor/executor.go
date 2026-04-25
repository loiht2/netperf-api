// Package executor orchestrates the iperf3 measurements across the cluster.
// It follows the Exec pattern: it never creates new pods; instead it uses
// client-go's remotecommand SPDY executor to run iperf3 inside the existing
// DaemonSet pods — the Kubernetes equivalent of `kubectl exec`.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/netperf/netperf-api/internal/k8sclient"
	"github.com/netperf/netperf-api/internal/scheduler"
	"github.com/netperf/netperf-api/internal/store"
	"github.com/netperf/netperf-api/pkg/iperf3"
)

const (
	// Namespace where both the DaemonSet and this API server live.
	Namespace = "netperf-api"

	// PodLabelSelector matches the iperf3 server DaemonSet pods.
	PodLabelSelector = "app=iperf3-server"

	// ContainerName is the container inside each DaemonSet pod.
	ContainerName = "iperf3"

	// IperfDuration is the seconds passed to iperf3 -t.
	IperfDuration = 10

	// roundCooldown is the mandatory pause between rounds to let TCP state drain.
	roundCooldown = 5 * time.Second

	// pairTimeout caps a single iperf3 exec: test duration + generous overhead.
	pairTimeout = 90 * time.Second

	// defaultWorkerLabel is the node label selector used to exclude control-plane
	// nodes from the measurement pool.  Override via the WORKER_NODE_LABEL env var.
	defaultWorkerLabel = "node-role.kubernetes.io/worker=true"

	// defaultServerPort is the iperf3 listener port (iperf3's own default).
	// Override via IPERF3_PORT to avoid conflicts when another iperf3 instance
	// already occupies 5201 on the same hosts (e.g. during e2e tests run alongside
	// a production DaemonSet).
	defaultServerPort = 5201
)

// workerNodeLabel returns the label selector used to filter worker nodes.
// Read from the WORKER_NODE_LABEL environment variable so it can be changed
// without recompiling (just update the Deployment env block in the YAML).
func workerNodeLabel() string {
	if l := os.Getenv("WORKER_NODE_LABEL"); l != "" {
		return l
	}
	return defaultWorkerLabel
}

// iperf3ServerPort returns the TCP port the iperf3 server DaemonSet is
// listening on. Reads IPERF3_PORT so integration tests can use an alternate
// port (e.g. 5202) without conflicting with a production DaemonSet on 5201.
func iperf3ServerPort() int {
	if p := os.Getenv("IPERF3_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return defaultServerPort
}

// BandwidthEntry holds the measured bandwidth for a single directed link.
// Error is non-empty when this specific pair failed; the consumer can detect
// partial failures without the whole matrix being discarded.
type BandwidthEntry struct {
	MbpsIngress float64 `json:"mbps_ingress,omitempty"` // bandwidth received (remote → this node)
	MbpsEgress  float64 `json:"mbps_egress,omitempty"`  // bandwidth sent (this node → remote)
	Error       string  `json:"error,omitempty"`        // non-empty when this pair failed
}

// pairResult captures all throughput figures from a single --bidir iperf3 exec.
// One exec populates two matrix cells: [source][target] and [target][source].
type pairResult struct {
	Source             string
	Target             string
	EgressMbps         float64 // sum_sent: source→target
	IngressMbps        float64 // sum_received: target→source received at source
	ReverseEgressMbps  float64 // sum_sent_bidir_reverse: target→source
	ReverseIngressMbps float64 // sum_received_bidir_reverse: source→target received at target
	Error              string
}

// Result is the final payload stored in the task once the run completes.
type Result struct {
	Nodes  []string                              `json:"nodes"`
	Matrix map[string]map[string]*BandwidthEntry `json:"matrix"`
}

// Executor holds the Kubernetes client and drives the full measurement lifecycle.
type Executor struct {
	client    *k8sclient.Client
	namespace string // K8s namespace where iperf3 DaemonSet pods live
}

// New constructs an Executor targeting the default "netperf" namespace.
func New(c *k8sclient.Client) *Executor {
	return &Executor{client: c, namespace: Namespace}
}

// NewForNamespace constructs an Executor targeting an arbitrary namespace.
// Used by integration tests to operate inside an isolated temporary namespace
// without touching the production "netperf" namespace.
func NewForNamespace(c *k8sclient.Client, ns string) *Executor {
	return &Executor{client: c, namespace: ns}
}

// CountReadyNodes returns the number of Ready worker nodes visible to the
// executor. Used by the API handler to compute an ETA before the measurement
// goroutine is launched.
func (e *Executor) CountReadyNodes(ctx context.Context) (int, error) {
	ips, _, err := e.readyNodeIPs(ctx)
	return len(ips), err
}

// Run is the entry point called from a background goroutine.
// It updates the task in the store as it progresses and guarantees the
// cancel func is removed from the store when it exits (deferred).
func (e *Executor) Run(ctx context.Context, taskID string, s *store.Store) {
	// Always remove the cancel func when we exit — whether we completed
	// naturally, failed, or were cancelled externally via the DELETE endpoint.
	defer s.DeleteCancel(taskID)

	started := time.Now()

	setStatus := func(status store.Status) {
		if t, ok := s.Get(taskID); ok {
			t.Status = status
			s.Set(taskID, t)
		}
	}

	setStatus(store.StatusRunning)

	result, err := e.run(ctx)

	t, _ := s.Get(taskID)
	switch {
	case err == nil:
		t.Duration = time.Since(started).String()
		t.Status = store.StatusCompleted
		t.Result = result
	case errors.Is(err, context.Canceled):
		// Context was cancelled by the DELETE endpoint.
		t.Status = store.StatusCanceled
		t.Error = "measurement cancelled by request"
	default:
		t.Status = store.StatusFailed
		t.Error = err.Error()
	}
	s.Set(taskID, t)
}

// run performs the full measurement and returns the aggregated result.
func (e *Executor) run(ctx context.Context) (*Result, error) {
	// ── 1. Discover Ready nodes and their internal IPs ──────────────────────
	nodeIPs, ipToName, err := e.readyNodeIPs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodeIPs) < 2 {
		return nil, fmt.Errorf("need at least 2 ready nodes, found %d", len(nodeIPs))
	}
	log.Printf("[executor] discovered %d ready nodes: %v", len(nodeIPs), nodeIPs)

	// ── 2. Map each node IP → the iperf3 DaemonSet pod running on it ────────
	nodePods, err := e.buildNodePodMap(ctx, ipToName)
	if err != nil {
		return nil, fmt.Errorf("mapping iperf3 pods to nodes: %w", err)
	}

	// ── 3. Generate the round-robin schedule ────────────────────────────────
	sched := scheduler.GenerateSchedule(nodeIPs)
	log.Printf("[executor] schedule: %d rounds for %d nodes", len(sched), len(nodeIPs))

	// ── 4. Execute rounds with WaitGroup-based barrier synchronisation ───────
	// Pre-initialise all row maps so concurrent readers never see a nil inner map.
	matrix := make(map[string]map[string]*BandwidthEntry, len(nodeIPs))
	for _, ip := range nodeIPs {
		matrix[ip] = make(map[string]*BandwidthEntry)
	}

	for roundIdx, round := range sched {
		log.Printf("[executor] round %d/%d — %d concurrent pairs", roundIdx+1, len(sched), len(round))

		// Pre-allocate a result slot per pair so goroutines write to distinct
		// indices without needing a mutex on the slice itself.
		roundResults := make([]pairResult, len(round))
		var wg sync.WaitGroup

		for pairIdx, pair := range round {
			wg.Add(1)
			go func(idx int, p scheduler.Pair) {
				defer wg.Done()
				// Give each individual exec its own tight deadline.
				pairCtx, cancel := context.WithTimeout(ctx, pairTimeout)
				defer cancel()
				roundResults[idx] = e.execPair(pairCtx, p, nodePods)
			}(pairIdx, pair)
		}

		// Barrier: wait for ALL pairs in this round before proceeding.
		wg.Wait()

		// Merge into matrix (sequential after wg.Wait — no races).
		// Each --bidir exec yields two directed entries from one measurement.
		// Errors are ALWAYS written into the matrix so callers can see exactly
		// which pair failed and why — never silently skipped.
		for _, pr := range roundResults {
			log.Printf("[executor] pair %s→%s: egress=%.1f ingress=%.1f revEgress=%.1f revIngress=%.1f err=%q",
				pr.Source, pr.Target,
				pr.EgressMbps, pr.IngressMbps,
				pr.ReverseEgressMbps, pr.ReverseIngressMbps,
				pr.Error)
			if pr.Error != "" {
				matrix[pr.Source][pr.Target] = &BandwidthEntry{Error: pr.Error}
				matrix[pr.Target][pr.Source] = &BandwidthEntry{Error: pr.Error}
				continue
			}
			matrix[pr.Source][pr.Target] = &BandwidthEntry{
				MbpsEgress:  pr.EgressMbps,
				MbpsIngress: pr.IngressMbps,
			}
			matrix[pr.Target][pr.Source] = &BandwidthEntry{
				MbpsEgress:  pr.ReverseEgressMbps,
				MbpsIngress: pr.ReverseIngressMbps,
			}
		}

		// Mandatory cooldown between rounds (skip after the final round).
		// select gives the full 5 s pause between rounds while still unblocking
		// immediately when the context is cancelled (e.g. DELETE /cancel).
		// Pair-level failures do NOT skip or shorten this cooldown.
		if roundIdx < len(sched)-1 {
			log.Printf("[executor] cooldown %v before next round", roundCooldown)
			select {
			case <-time.After(roundCooldown):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	return &Result{
		Nodes:  nodeIPs,
		Matrix: matrix,
	}, nil
}

// execPair runs `iperf3 -c <Target> -t 10 --bidir -J` inside the source pod
// and returns a pairResult containing throughput for both directed links.
//
// Every failure path sets pr.Error so the caller can write a diagnostic entry
// into the matrix — no error is ever silently swallowed or converted to zeros.
//
// Field mapping from iperf3 --bidir JSON (measured at the client/source pod):
//
//	end.sum_sent                   → EgressMbps         (source→target sender)
//	end.sum_received               → IngressMbps        (source←target receiver)
//	end.sum_sent_bidir_reverse     → ReverseEgressMbps  (target→source sender)
//	end.sum_received_bidir_reverse → ReverseIngressMbps (target←source receiver)
func (e *Executor) execPair(ctx context.Context, p scheduler.Pair, nodePods map[string]string) pairResult {
	pr := pairResult{Source: p.Source, Target: p.Target}

	srcPod, ok := nodePods[p.Source]
	if !ok {
		pr.Error = fmt.Sprintf("no running iperf3 pod found for source node %s", p.Source)
		return pr
	}

	port := iperf3ServerPort()
	cmd := []string{
		"iperf3",
		"-c", p.Target,
		"-p", strconv.Itoa(port),
		"-t", fmt.Sprintf("%d", IperfDuration),
		"--bidir",
		"-J", // JSON output
	}

	stdout, stderr, execErr := e.podExec(ctx, srcPod, cmd)
	log.Printf("[executor] pair %s→%s: stdout=%d bytes stderr=%d bytes err=%v",
		p.Source, p.Target, len(stdout), len(stderr), execErr)
	// Log the raw stdout unconditionally so pod logs always show what iperf3
	// actually returned — crucial for diagnosing JSON mapping failures.
	log.Printf("[executor] pair %s→%s: raw stdout:\n%s", p.Source, p.Target, stdout)

	// No stdout at all — surface whatever exec-level error we have.
	if len(stdout) == 0 {
		if execErr != nil {
			pr.Error = fmt.Sprintf("exec failed (no output): %v | stderr: %s", execErr, stderr)
		} else {
			pr.Error = "iperf3 produced no output"
		}
		return pr
	}

	// iperf3 -J writes a JSON document even on connection failure (it embeds
	// the reason in the top-level "error" field).  extractJSON inside Parse
	// strips any warning lines that precede the opening '{'.
	out, parseErr := iperf3.Parse([]byte(stdout))
	if parseErr != nil {
		// JSON parse failed — include as much diagnostic context as possible.
		if execErr != nil {
			pr.Error = fmt.Sprintf("JSON parse failed: %v | exec error: %v | stderr: %s",
				parseErr, execErr, stderr)
		} else {
			pr.Error = fmt.Sprintf("JSON parse failed: %v | raw stdout: %.300s",
				parseErr, stdout)
		}
		return pr
	}

	// iperf3 itself reported an error (e.g. "Connection refused").
	if out.Error != "" {
		pr.Error = fmt.Sprintf("iperf3 reported: %s", out.Error)
		return pr
	}

	// Tolerant bidir extraction — missing fields return 0, not a parse error.
	bidir, bidirErr := iperf3.ParseEnd(out.End)
	if bidirErr != nil {
		pr.Error = fmt.Sprintf("bidir parse failed: %v", bidirErr)
		return pr
	}

	pr.EgressMbps         = iperf3.BitsToMbps(bidir.FwdSentBps)
	pr.IngressMbps        = iperf3.BitsToMbps(bidir.FwdRecvBps)
	pr.ReverseEgressMbps  = iperf3.BitsToMbps(bidir.RevSentBps)
	pr.ReverseIngressMbps = iperf3.BitsToMbps(bidir.RevRecvBps)
	return pr
}

// podExec is the core "kubectl exec" equivalent.
//
// How it works:
//  1. Build the pod/exec subresource URL using the typed REST client.
//     VersionedParams serialises PodExecOptions into query parameters
//     (command=iperf3&command=-c&command=... &stdout=true &stderr=true ...)
//  2. NewSPDYExecutor upgrades the HTTP connection to SPDY — the same
//     multiplexed framing protocol that `kubectl exec` uses.  SPDY carries
//     stdin, stdout, stderr, and resize channels over a single TCP connection.
//  3. StreamWithContext drives the SPDY session, forwarding IO until the
//     remote process exits or the context is cancelled.
func (e *Executor) podExec(ctx context.Context, podName string, cmd []string) (string, string, error) {
	// Build the URL for the exec subresource.
	req := e.client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(e.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: ContainerName,
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec) // ParameterCodec serialises the struct to URL query params

	// NewSPDYExecutor negotiates an HTTP→SPDY upgrade to the API server.
	// The REST config supplies TLS credentials and the API server address.
	exec, err := remotecommand.NewSPDYExecutor(e.client.RestConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		// Return stdout anyway: iperf3 may have written JSON before failing.
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// ── Node / Pod discovery helpers ─────────────────────────────────────────────

// readyNodeIPs returns the InternalIP of every Ready worker node, plus a
// reverse map (IP → node name) used to locate DaemonSet pods below.
// Control-plane nodes are excluded via the WORKER_NODE_LABEL selector so
// we only measure bandwidth across the data-plane node pool.
func (e *Executor) readyNodeIPs(ctx context.Context) (ips []string, ipToName map[string]string, err error) {
	label := workerNodeLabel()
	log.Printf("[executor] listing nodes with label selector: %q", label)
	nodes, err := e.client.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return nil, nil, err
	}

	ipToName = make(map[string]string)
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !nodeIsReady(n) {
			continue
		}
		ip := nodeInternalIP(n)
		if ip == "" {
			log.Printf("[executor] node %s has no InternalIP, skipping", n.Name)
			continue
		}
		ips = append(ips, ip)
		ipToName[ip] = n.Name
	}
	return ips, ipToName, nil
}

func nodeIsReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func nodeInternalIP(n *corev1.Node) string {
	for _, addr := range n.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

// buildNodePodMap returns a map from nodeIP → iperf3 DaemonSet pod name.
// Only Running pods are included; a missing entry means that node is skipped.
func (e *Executor) buildNodePodMap(ctx context.Context, ipToName map[string]string) (map[string]string, error) {
	pods, err := e.client.Clientset.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: PodLabelSelector,
	})
	if err != nil {
		return nil, err
	}

	// Invert ipToName so we can look up by node name.
	nameToIP := make(map[string]string, len(ipToName))
	for ip, name := range ipToName {
		nameToIP[name] = ip
	}

	result := make(map[string]string)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			log.Printf("[executor] pod %s on node %s is %s, skipping", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
			continue
		}
		if ip, ok := nameToIP[pod.Spec.NodeName]; ok {
			result[ip] = pod.Name
		}
	}
	return result, nil
}
