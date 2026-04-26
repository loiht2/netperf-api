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

	// IperfInitialDuration is the iperf3 -t value (seconds) for the first attempt.
	// Exported so the API handler can compute the happy-path ETA.
	IperfInitialDuration = 10

	// iperfRetryDuration is the iperf3 -t value (seconds) for retry attempts 2–4.
	// Shorter to recover quickly after a transient SPDY stream interruption.
	iperfRetryDuration = 5

	// iperfInitialTimeout is the per-attempt context deadline for the first attempt:
	// full 10 s test + generous margin for SPDY negotiation and teardown.
	iperfInitialTimeout = 15 * time.Second

	// iperfRetryTimeout is the per-attempt context deadline for retry attempts:
	// 5 s test + 3 s margin.
	iperfRetryTimeout = 8 * time.Second

	// retryWait is the fixed pause between consecutive attempts for the same pair.
	// Long enough for the Tailscale reconnect to stabilise before we retry.
	retryWait = 3 * time.Second

	// maxAttempts is the total number of exec attempts per pair (1 initial + 3 retries).
	maxAttempts = 4

	// roundCooldown is the mandatory pause between rounds to let TCP state drain.
	roundCooldown = 5 * time.Second

	// defaultServerPort is the iperf3 listener port (iperf3's own default).
	// Override via IPERF3_PORT to avoid conflicts when another iperf3 instance
	// already occupies 5201 on the same hosts (e.g. during e2e tests run alongside
	// a production DaemonSet).
	defaultServerPort = 5201
)

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

// NodeSnapshot is a point-in-time record of which nodes are ready for
// measurement when a task is accepted. It is captured once per POST request
// and passed to the background goroutine unchanged, so cluster churn during a
// run (pods restarting, nodes draining) does not affect the in-flight task.
// The next POST will capture a fresh snapshot with the updated cluster state.
type NodeSnapshot struct {
	IPs      []string          // ordered list of node HostIPs ready for testing
	PodNames map[string]string // nodeIP → iperf3 DaemonSet pod name (for exec)
}

// BandwidthData is one cell of the directional matrix.
//
// Mbps is the bandwidth that the column node (Target) successfully received
// from the row node (Source). Error is non-empty when this specific directed
// link failed; in that case Mbps is 0.
type BandwidthData struct {
	Mbps  float64 `json:"mbps"`
	Error string  `json:"error,omitempty"`
}

// pairResult captures the two directional throughputs produced by a single
// --bidir iperf3 exec. From this one struct we populate exactly TWO matrix
// cells: matrix[Source][Target] and matrix[Target][Source].
type pairResult struct {
	Source       string
	Target       string
	ToTargetMbps float64 // bandwidth Target received from Source — matrix[Source][Target].Mbps
	ToSourceMbps float64 // bandwidth Source received from Target — matrix[Target][Source].Mbps
	Error        string
}

// Result is the final payload stored in the task once the run completes.
//
// Matrix is a directional N×N adjacency matrix:
//
//	row    = sender (Source)
//	column = receiver (Target)
//	cell   = bandwidth that Target successfully received from Source (Mbps)
//
// Diagonal cells (matrix[X][X]) are absent — a node never tests against itself.
// matrix[A][B] and matrix[B][A] are independent values populated from the
// same --bidir exec but representing the two opposite directions of that link.
type Result struct {
	Nodes  []string                              `json:"nodes"`
	Matrix map[string]map[string]*BandwidthData `json:"matrix"`
}

// Executor holds the Kubernetes client and drives the full measurement lifecycle.
type Executor struct {
	client    *k8sclient.Client
	namespace string // K8s namespace where iperf3 DaemonSet pods live
}

// New constructs an Executor targeting the default namespace.
func New(c *k8sclient.Client) *Executor {
	return &Executor{client: c, namespace: Namespace}
}

// NewForNamespace constructs an Executor targeting an arbitrary namespace.
// Used by integration tests to operate inside an isolated temporary namespace
// without touching the production namespace.
func NewForNamespace(c *k8sclient.Client, ns string) *Executor {
	return &Executor{client: c, namespace: ns}
}

// DiscoverNodes performs a pod-based, data-plane-aware discovery of all nodes
// that are ready to participate in a measurement. A node is included only when
// its iperf3 DaemonSet pod satisfies BOTH conditions:
//
//   - Phase == Running  (the container process has started)
//   - PodReady == True  (the readiness probe has passed — iperf3 is accepting connections)
//
// The node's HostIP (the real node IP, identical to the DaemonSet pod IP because
// hostNetwork: true is set) is used as both the measurement target address and
// the map key for pod lookup during exec.
//
// This approach is strictly superior to listing Nodes by label:
//   - Eliminates races where a node is labelled but its pod has not started yet.
//   - Catches pods that are Running but failing readiness (CrashLoop, bad config).
//   - Requires no node-level RBAC permissions (pods/list is sufficient).
func (e *Executor) DiscoverNodes(ctx context.Context) (*NodeSnapshot, error) {
	pods, err := e.client.Clientset.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: PodLabelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing iperf3 DaemonSet pods: %w", err)
	}

	total := len(pods.Items)
	log.Printf("[executor] discovery: %d pod(s) found with selector %q in namespace %q",
		total, PodLabelSelector, e.namespace)

	snapshot := &NodeSnapshot{
		IPs:      []string{},
		PodNames: make(map[string]string),
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			nodeName = "<unscheduled>"
		}

		switch {
		case pod.Status.Phase != corev1.PodRunning:
			log.Printf("[executor] skip pod %s (node %s): phase=%s, want Running",
				pod.Name, nodeName, pod.Status.Phase)

		case !podIsReady(pod):
			log.Printf("[executor] skip pod %s (node %s): PodReady condition is not True",
				pod.Name, nodeName)

		case pod.Status.HostIP == "":
			log.Printf("[executor] skip pod %s (node %s): HostIP is empty",
				pod.Name, nodeName)

		default:
			hostIP := pod.Status.HostIP
			if _, dup := snapshot.PodNames[hostIP]; dup {
				log.Printf("[executor] skip pod %s (node %s): HostIP %s already registered",
					pod.Name, nodeName, hostIP)
				continue
			}
			snapshot.IPs = append(snapshot.IPs, hostIP)
			snapshot.PodNames[hostIP] = pod.Name
		}
	}

	ready := len(snapshot.IPs)
	log.Printf("[executor] discovery complete — total pods: %d | Running+Ready: %d | nodes: %v",
		total, ready, snapshot.IPs)

	return snapshot, nil
}

// podIsReady returns true when the pod's PodReady condition is True.
func podIsReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Run is the entry point called from a background goroutine.
// It takes the NodeSnapshot captured at POST time so the measurement uses a
// stable, consistent view of the cluster for its entire duration.
// It updates the task in the store as it progresses and guarantees the
// cancel func is removed from the store when it exits (deferred).
func (e *Executor) Run(ctx context.Context, taskID string, s *store.Store, snapshot *NodeSnapshot) {
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

	result, err := e.run(ctx, snapshot)

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

// run performs the full measurement using the pre-captured node snapshot.
// The snapshot is the single source of truth for node IPs and pod names for
// the duration of this task; no further cluster API calls for node discovery
// are made after this point.
func (e *Executor) run(ctx context.Context, snapshot *NodeSnapshot) (*Result, error) {
	nodeIPs := snapshot.IPs
	if len(nodeIPs) < 2 {
		return nil, fmt.Errorf("need at least 2 ready nodes in snapshot, found %d", len(nodeIPs))
	}

	log.Printf("[executor] starting measurement: %d nodes %v", len(nodeIPs), nodeIPs)

	// ── Generate the round-robin schedule ────────────────────────────────────
	sched := scheduler.GenerateSchedule(nodeIPs)
	log.Printf("[executor] schedule: %d rounds for %d nodes", len(sched), len(nodeIPs))

	// ── Allocate the directional matrix ──────────────────────────────────────
	// One row per node, each row is an inner map keyed by Target. Diagonal
	// cells are deliberately never inserted — a node never measures itself.
	matrix := make(map[string]map[string]*BandwidthData, len(nodeIPs))
	for _, ip := range nodeIPs {
		matrix[ip] = make(map[string]*BandwidthData, len(nodeIPs)-1)
	}

	for roundIdx, round := range sched {
		log.Printf("[executor] round %d/%d — %d concurrent pair(s)", roundIdx+1, len(sched), len(round))

		// Pre-allocate a result slot per pair so goroutines write to distinct
		// indices without needing a mutex on the slice itself.
		roundResults := make([]pairResult, len(round))
		var wg sync.WaitGroup

		for pairIdx, pair := range round {
			wg.Add(1)
			go func(idx int, p scheduler.Pair) {
				defer wg.Done()
				// execPair manages its own per-attempt timeouts internally.
				// Pass the task context directly so DELETE cancellation propagates.
				roundResults[idx] = e.execPair(ctx, p, snapshot.PodNames)
			}(pairIdx, pair)
		}

		// Barrier: wait for ALL pairs in this round before proceeding.
		wg.Wait()

		// Each --bidir exec produces TWO directional matrix cells, one per
		// direction. Errors propagate to BOTH cells (we cannot trust either
		// direction when the exec itself failed) and are written explicitly so
		// callers can pinpoint exactly which directed link is broken.
		for _, pr := range roundResults {
			if pr.Error != "" {
				log.Printf("[Round %d] Pair %s <-> %s FAILED: %s",
					roundIdx+1, pr.Source, pr.Target, pr.Error)
				matrix[pr.Source][pr.Target] = &BandwidthData{Error: pr.Error}
				matrix[pr.Target][pr.Source] = &BandwidthData{Error: pr.Error}
				continue
			}
			log.Printf("[Round %d] %s→%s = %.2f Mbps  |  %s→%s = %.2f Mbps",
				roundIdx+1,
				pr.Source, pr.Target, pr.ToTargetMbps,
				pr.Target, pr.Source, pr.ToSourceMbps)
			matrix[pr.Source][pr.Target] = &BandwidthData{Mbps: pr.ToTargetMbps}
			matrix[pr.Target][pr.Source] = &BandwidthData{Mbps: pr.ToSourceMbps}
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

// execPair runs iperf3 inside the source pod and returns a pairResult with the
// two receiver-side throughput values — one per direction of the link.
//
// Retry strategy (resilient to transient SPDY stream interruptions):
//
//	Attempt 1 : iperf3 -t 10, per-attempt timeout 15 s
//	Attempts 2–4 : iperf3 -t  5, per-attempt timeout  8 s
//	Between attempts: 3 s context-aware wait
//
// The root cause of transient 0-byte output is a Tailscale reconnect event
// that severs the API-server ↔ Kubelet SPDY leg mid-test. The API server
// performs a graceful TCP FIN, which client-go's spdystream interprets as a
// clean EOF → nil error. iperf3 -J buffers all output until test completion,
// so a severed stream yields stdout="" with err=nil. The shorter retry
// duration lets the pair recover before the round cooldown expires.
//
// Every failure path sets pr.Error explicitly — no error is silently swallowed.
//
// Field mapping from iperf3 --bidir JSON:
//
//	end.sum_received               → ToTargetMbps
//	end.sum_received_bidir_reverse → ToSourceMbps
func (e *Executor) execPair(ctx context.Context, p scheduler.Pair, nodePods map[string]string) pairResult {
	pr := pairResult{Source: p.Source, Target: p.Target}

	srcPod, ok := nodePods[p.Source]
	if !ok {
		pr.Error = fmt.Sprintf("no running iperf3 pod found for source node %s", p.Source)
		return pr
	}

	port := iperf3ServerPort()
	var lastErr string

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Inter-attempt wait (skip before the first attempt).
		if attempt > 1 {
			prevDuration := IperfInitialDuration
			if attempt > 2 {
				prevDuration = iperfRetryDuration
			}
			log.Printf("[Pair %s<->%s] Attempt %d/%d (%ds) failed: %s. Retrying (Attempt %d/%d, %ds)...",
				p.Source, p.Target, attempt-1, maxAttempts, prevDuration, lastErr,
				attempt, maxAttempts, iperfRetryDuration)
			select {
			case <-time.After(retryWait):
			case <-ctx.Done():
				pr.Error = fmt.Sprintf("cancelled before attempt %d: %v", attempt, ctx.Err())
				return pr
			}
		}

		duration := IperfInitialDuration
		timeout := iperfInitialTimeout
		if attempt > 1 {
			duration = iperfRetryDuration
			timeout = iperfRetryTimeout
		}

		cmd := []string{
			"iperf3",
			"-c", p.Target,
			"-p", strconv.Itoa(port),
			"-t", strconv.Itoa(duration),
			"--bidir",
			"-J",
		}

		attemptCtx, attemptCancel := context.WithTimeout(ctx, timeout)
		stdout, stderr, execErr := e.podExec(attemptCtx, srcPod, cmd)
		attemptCancel()

		log.Printf("[executor] pair %s→%s attempt %d/%d (%ds): stdout=%d bytes stderr=%d bytes err=%v",
			p.Source, p.Target, attempt, maxAttempts, duration, len(stdout), len(stderr), execErr)

		// Empty stdout — the SPDY stream closed before iperf3 flushed its JSON.
		if len(stdout) == 0 {
			if execErr != nil {
				lastErr = fmt.Sprintf("exec failed (no output): %v | stderr: %s", execErr, stderr)
			} else {
				lastErr = "iperf3 produced no output (SPDY stream closed mid-test)"
			}
			// If the outer context was cancelled, stop immediately — no retry.
			if ctx.Err() != nil {
				pr.Error = fmt.Sprintf("cancelled during attempt %d: %v", attempt, ctx.Err())
				return pr
			}
			continue
		}

		// extractJSON strips warning lines before the opening '{'.
		out, parseErr := iperf3.Parse([]byte(stdout))
		if parseErr != nil {
			log.Printf("[executor] pair %s→%s attempt %d: PARSE ERROR — raw stdout (truncated to 4KB):\n%.4096s",
				p.Source, p.Target, attempt, stdout)
			if execErr != nil {
				lastErr = fmt.Sprintf("JSON parse failed: %v | exec error: %v | stderr: %s", parseErr, execErr, stderr)
			} else {
				lastErr = fmt.Sprintf("JSON parse failed: %v", parseErr)
			}
			continue
		}

		// iperf3 itself reported an error (e.g. "Connection refused").
		if out.Error != "" {
			lastErr = fmt.Sprintf("iperf3 reported: %s", out.Error)
			continue
		}

		// Tolerant bidir extraction — missing fields return 0, not a parse error.
		bidir, bidirErr := iperf3.ParseEnd(out.End)
		if bidirErr != nil {
			lastErr = fmt.Sprintf("bidir parse failed: %v", bidirErr)
			continue
		}

		// ── Success ──────────────────────────────────────────────────────────────
		pr.ToTargetMbps = iperf3.BitsToMbps(bidir.ToTargetBps)
		pr.ToSourceMbps = iperf3.BitsToMbps(bidir.ToSourceBps)
		return pr
	}

	// All attempts exhausted.
	log.Printf("[Pair %s<->%s] All %d attempts failed. Last error: %s",
		p.Source, p.Target, maxAttempts, lastErr)
	pr.Error = fmt.Sprintf("failed after %d attempts (network unstable): %s", maxAttempts, lastErr)
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
