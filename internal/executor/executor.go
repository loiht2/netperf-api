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

	// IperfInitialDuration is the iperf3 -t value (seconds).
	// Exported so the API handler can compute the ETA.
	IperfInitialDuration = 10

	// ── Store-and-Fetch timing constants ─────────────────────────────────────

	// iperfFileWait is the minimum elapsed time from the start of Step A before
	// we attempt to read the result file (Step B). iperf3 -t 10 needs at least
	// 10 s to run; the 1 s buffer gives it time to flush and close the file.
	// If Step A's SPDY stream drops at, say, 3 s, the iperf3 process continues
	// inside the container — we simply wait for the remaining 8 s here.
	iperfFileWait = 11 * time.Second

	// iperfStepATimeout is the context deadline for the Step A exec.
	// Generous enough to cover the full iperf3 run plus SPDY setup/teardown.
	// A Tailscale flap during this window returns early with an error, but the
	// iperf3 process keeps running — the wait below compensates.
	iperfStepATimeout = 15 * time.Second

	// iperfFetchTimeout is the per-attempt deadline for the Step B cat exec.
	// Short because cat on an existing file returns immediately; the timeout
	// guards only against SPDY negotiation hanging.
	iperfFetchTimeout = 3 * time.Second

	// iperfFetchRetries is the number of additional fetch attempts after the
	// initial one. Total attempts = iperfFetchRetries + 1 = 4.
	iperfFetchRetries = 3

	// iperfFetchRetryWait is the pause between consecutive Step B attempts.
	iperfFetchRetryWait = 1 * time.Second

	// iperfCleanupTimeout caps the Step C rm -f exec so a stuck cleanup
	// does not block the goroutine indefinitely.
	iperfCleanupTimeout = 5 * time.Second

	// roundCooldown is the mandatory pause between rounds to let TCP state drain.
	roundCooldown = 5 * time.Second

	// defaultServerPort is the iperf3 listener port (iperf3's own default).
	// Override via IPERF3_PORT to avoid conflicts when another iperf3 instance
	// already occupies 5201 on the same hosts (e.g. during e2e tests run alongside
	// a production DaemonSet).
	defaultServerPort = 5201

)

// ExecFunc is the signature of the pod-exec layer used by execPair.
// In production this is *Executor.podExec, which performs a real SPDY
// remotecommand stream. Tests inject a deterministic fake via SetExecFunc to
// drive the executor without a Kubernetes cluster.
type ExecFunc func(ctx context.Context, podName string, cmd []string) (stdout, stderr string, err error)

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
// Matrix is a directional adjacency map keyed by [Source][Target].
//
//	row    = sender (Source)
//	column = receiver (Target)
//	cell   = bandwidth that Target successfully received from Source (Mbps)
//
// Each source row contains N-1 entries — one per peer, never itself.
// matrix[A][B] and matrix[B][A] are independent values populated from the
// same --bidir exec but representing the two opposite directions of that link.
type Result struct {
	Nodes  []string                              `json:"nodes"`
	Matrix map[string]map[string]*BandwidthData `json:"matrix"`
}

// Executor holds the Kubernetes client and drives the full measurement lifecycle.
//
// The execFunc, fileWaitOverride, fetchRetryWaitOverride, and roundCooldownOverride
// fields exist solely to support unit tests: production code uses the SPDY-based
// podExec and the package-level timing constants. See SetExecFunc and
// SetTimingForTesting for the test-only entry points.
type Executor struct {
	client    *k8sclient.Client
	namespace string // K8s namespace where iperf3 DaemonSet pods live

	// execFunc is the function used to run commands inside DaemonSet pods.
	// Defaults to (*Executor).podExec; tests override via SetExecFunc.
	execFunc ExecFunc

	// Timing overrides — zero means "use the package-level default".
	fileWaitOverride       time.Duration
	fetchRetryWaitOverride time.Duration
	roundCooldownOverride  time.Duration
}

// New constructs an Executor targeting the default namespace.
func New(c *k8sclient.Client) *Executor {
	e := &Executor{client: c, namespace: Namespace}
	e.execFunc = e.podExec
	return e
}

// NewForNamespace constructs an Executor targeting an arbitrary namespace.
// Used by integration tests to operate inside an isolated temporary namespace
// without touching the production namespace.
func NewForNamespace(c *k8sclient.Client, ns string) *Executor {
	e := &Executor{client: c, namespace: ns}
	e.execFunc = e.podExec
	return e
}

// SetExecFunc overrides the pod-exec layer with a custom function.
// Production code never calls this; it exists so unit tests can replace the
// SPDY remotecommand stream with a deterministic fake.
func (e *Executor) SetExecFunc(fn ExecFunc) {
	e.execFunc = fn
}

// SetTimingForTesting reduces the inter-step waits so unit tests do not have
// to spend 11 s on every Step A or 1 s on every fetch retry. A zero value for
// any parameter restores the production default for that timing. Production
// code never calls this.
func (e *Executor) SetTimingForTesting(fileWait, fetchRetryWait, roundCooldown time.Duration) {
	e.fileWaitOverride = fileWait
	e.fetchRetryWaitOverride = fetchRetryWait
	e.roundCooldownOverride = roundCooldown
}

// effFileWait returns the effective file-wait duration after applying overrides.
func (e *Executor) effFileWait() time.Duration {
	if e.fileWaitOverride > 0 {
		return e.fileWaitOverride
	}
	return iperfFileWait
}

// effFetchRetryWait returns the effective fetch-retry wait after overrides.
func (e *Executor) effFetchRetryWait() time.Duration {
	if e.fetchRetryWaitOverride > 0 {
		return e.fetchRetryWaitOverride
	}
	return iperfFetchRetryWait
}

// effRoundCooldown returns the effective round cooldown after overrides.
func (e *Executor) effRoundCooldown() time.Duration {
	if e.roundCooldownOverride > 0 {
		return e.roundCooldownOverride
	}
	return roundCooldown
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

	result, err := e.run(ctx, taskID, snapshot)

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
// are made after this point. taskID is propagated into each pair exec so
// result files in /tmp can be named uniquely per task and round.
func (e *Executor) run(ctx context.Context, taskID string, snapshot *NodeSnapshot) (*Result, error) {
	nodeIPs := snapshot.IPs
	if len(nodeIPs) < 2 {
		return nil, fmt.Errorf("need at least 2 ready nodes in snapshot, found %d", len(nodeIPs))
	}

	log.Printf("[executor] starting measurement: %d nodes %v", len(nodeIPs), nodeIPs)

	// ── Generate the round-robin schedule ────────────────────────────────────
	sched := scheduler.GenerateSchedule(nodeIPs)
	log.Printf("[executor] schedule: %d rounds for %d nodes", len(sched), len(nodeIPs))

	// ── Allocate the directional matrix ──────────────────────────────────────
	// One row per node; each row will hold N-1 measured entries (all peers).
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
				roundResults[idx] = e.execPair(ctx, taskID, roundIdx, p, snapshot.PodNames)
			}(pairIdx, pair)
		}

		// Barrier: wait for ALL pairs in this round before proceeding.
		wg.Wait()

		// If the parent context was cancelled at any point during this round
		// (DELETE /cancel, server shutdown, etc.), surface that error to the
		// caller now. Without this check, a cancellation that landed during a
		// round's pair execution would be buried in an individual matrix cell's
		// error string, and the task would terminate with status=completed even
		// though the user explicitly cancelled it. The integration test
		// TestE2E_CancelMidFlight depends on this status transitioning to
		// "canceled" rather than "completed".
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

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
			cd := e.effRoundCooldown()
			log.Printf("[executor] cooldown %v before next round", cd)
			select {
			case <-time.After(cd):
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

// execPair measures bandwidth for one pair using the Store-and-Fetch pattern,
// which decouples iperf3 execution from SPDY stream liveness. It runs three
// sequential exec steps:
//
//	Step A — Run & Redirect: execute iperf3 inside the source pod and redirect
//	         its JSON output to a file in /tmp (backed by emptyDir). If the
//	         SPDY stream drops mid-test the iperf3 process continues running
//	         inside the container; we simply wait for the minimum test window
//	         before proceeding to Step B.
//
//	Step B — Fetch: cat the result file back to the executor. This step has its
//	         own retry loop (up to iperfFetchRetries additional attempts) because
//	         the cat stream itself may be interrupted by a Tailscale reconnect.
//
//	Step C — Cleanup: rm -f the result file immediately after a successful fetch
//	         (or after all fetch attempts are exhausted) to prevent emptyDir disk
//	         exhaustion. Always runs via defer using context.Background() so that
//	         task-level cancellation does not skip the cleanup.
//
// Why this beats fast-retry on the iperf3 exec directly:
//
//	A Tailscale reconnect severs the API-server ↔ Kubelet SPDY leg with a
//	graceful TCP FIN. client-go's spdystream sees this as a clean EOF → nil
//	error; iperf3 -J buffers ALL output until the test completes, so the
//	executor sees stdout="" with err=nil. Retrying the whole iperf3 command
//	wastes 5–10 s per retry and may still race the next Tailscale flap.
//	By redirecting output to a file we make the measurement independent of
//	stream liveness: iperf3 finishes, flushes the file, and we retrieve it
//	over a fresh SPDY connection (Step B).
//
// Field mapping from iperf3 --bidir JSON:
//
//	end.sum_received               → ToTargetMbps
//	end.sum_received_bidir_reverse → ToSourceMbps
func (e *Executor) execPair(ctx context.Context, taskID string, roundIdx int, p scheduler.Pair, nodePods map[string]string) pairResult {
	pr := pairResult{Source: p.Source, Target: p.Target}

	srcPod, ok := nodePods[p.Source]
	if !ok {
		pr.Error = fmt.Sprintf("no running iperf3 pod found for source node %s", p.Source)
		return pr
	}

	port := iperf3ServerPort()

	// Filename is unique per task × round × pair; dots in IPs are valid in
	// Linux filenames and no shell interpretation issue arises because we pass
	// the full shell command as a single argument to "sh -c".
	filename := fmt.Sprintf("/tmp/iperf_%s_R%d_%s_%s.json",
		taskID, roundIdx+1, p.Source, p.Target)

	// ── Step C: Cleanup (deferred — runs even if parsing fails or ctx cancelled) ─
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), iperfCleanupTimeout)
		defer cleanCancel()
		_, _, _ = e.execFunc(cleanCtx, srcPod, []string{"rm", "-f", filename})
		log.Printf("[executor] pair %s→%s: cleaned up %s", p.Source, p.Target, filename)
	}()

	// ── Step A: Run & Redirect ────────────────────────────────────────────────
	// Redirect iperf3's JSON stdout to a file; stderr goes to /dev/null because
	// iperf3 -J embeds application errors in the JSON payload on stdout.
	// The exec stream returning early (due to a Tailscale flap) is non-fatal:
	// iperf3 keeps running inside the container and eventually writes the file.
	shellCmd := fmt.Sprintf(
		"iperf3 -c %s -p %d -J -t %d --bidir > %s 2>/dev/null",
		p.Target, port, IperfInitialDuration, filename,
	)
	stepACtx, stepACancel := context.WithTimeout(ctx, iperfStepATimeout)
	stepAStart := time.Now()
	_, _, stepAErr := e.execFunc(stepACtx, srcPod, []string{"sh", "-c", shellCmd})
	stepACancel()
	stepAElapsed := time.Since(stepAStart)

	log.Printf("[executor] pair %s→%s Step A: elapsed=%v err=%v",
		p.Source, p.Target, stepAElapsed.Round(time.Millisecond), stepAErr)

	// Guarantee the file is fully written before we attempt to read it.
	// If the stream returned early (e.g. Tailscale flap at 3 s), iperf3 still
	// needs up to 10 s total; we wait for the remainder of the file-wait window.
	fileWait := e.effFileWait()
	if stepAElapsed < fileWait {
		remaining := fileWait - stepAElapsed
		log.Printf("[executor] pair %s→%s: Step A returned after %v; waiting %v more before fetch",
			p.Source, p.Target,
			stepAElapsed.Round(time.Millisecond),
			remaining.Round(time.Millisecond))
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
			pr.Error = fmt.Sprintf("cancelled while waiting for iperf3 to finish: %v", ctx.Err())
			return pr
		}
	}

	// ── Step B: Fetch the Results (with retry) ────────────────────────────────
	// cat the result file over a fresh SPDY connection. Retry on empty response
	// (file not yet written, or cat stream itself was dropped by a Tailscale
	// flap). After all attempts, an empty response means the test genuinely failed.
	maxFetchAttempts := iperfFetchRetries + 1
	fetchWait := e.effFetchRetryWait()
	var stdout string
	for attempt := 1; attempt <= maxFetchAttempts; attempt++ {
		fetchCtx, fetchCancel := context.WithTimeout(ctx, iperfFetchTimeout)
		out, _, fetchErr := e.execFunc(fetchCtx, srcPod, []string{"cat", filename})
		fetchCancel()

		log.Printf("[executor] pair %s→%s Step B attempt %d/%d: stdout=%d bytes err=%v",
			p.Source, p.Target, attempt, maxFetchAttempts, len(out), fetchErr)

		if len(out) > 0 {
			stdout = out
			break
		}

		if ctx.Err() != nil {
			pr.Error = fmt.Sprintf("cancelled during fetch attempt %d: %v", attempt, ctx.Err())
			return pr
		}

		if attempt < maxFetchAttempts {
			log.Printf("[executor] pair %s→%s: output not ready on fetch attempt %d/%d, waiting %v",
				p.Source, p.Target, attempt, maxFetchAttempts, fetchWait)
			select {
			case <-time.After(fetchWait):
			case <-ctx.Done():
				pr.Error = fmt.Sprintf("cancelled while waiting for fetch retry %d: %v", attempt+1, ctx.Err())
				return pr
			}
		}
	}

	if stdout == "" {
		pr.Error = fmt.Sprintf("iperf3 output file empty or missing after %d fetch attempts (test may have failed to start)", maxFetchAttempts)
		return pr
	}

	// ── Parse ─────────────────────────────────────────────────────────────────
	// extractJSON (called inside Parse) strips any warning lines before the
	// opening '{' so the parser handles non-clean iperf3 builds.
	parsed, parseErr := iperf3.Parse([]byte(stdout))
	if parseErr != nil {
		log.Printf("[executor] pair %s→%s: PARSE ERROR — raw stdout (truncated to 4KB):\n%.4096s",
			p.Source, p.Target, stdout)
		pr.Error = fmt.Sprintf("JSON parse failed: %v", parseErr)
		return pr
	}
	if parsed.Error != "" {
		pr.Error = fmt.Sprintf("iperf3 reported: %s", parsed.Error)
		return pr
	}

	bidir, bidirErr := iperf3.ParseEnd(parsed.End)
	if bidirErr != nil {
		pr.Error = fmt.Sprintf("bidir parse failed: %v", bidirErr)
		return pr
	}

	pr.ToTargetMbps = iperf3.BitsToMbps(bidir.ToTargetBps)
	pr.ToSourceMbps = iperf3.BitsToMbps(bidir.ToSourceBps)
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
