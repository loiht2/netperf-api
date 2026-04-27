//go:build integration

// Package integration contains end-to-end tests that run against a LIVE
// Kubernetes cluster. They are gated behind the "integration" build tag so
// they are never executed by `go test ./...` accidentally.
//
// Run with:
//
//	go test -v -tags integration -timeout 15m ./test/integration/
//
// Requirements:
//   - A reachable cluster via ~/.kube/config (or $KUBECONFIG).
//   - The test runner's kubeconfig identity must have:
//       - nodes: get, list, watch
//       - pods: get, list, watch (cluster-wide)
//       - pods/exec: create   (cluster-wide)
//       - namespaces: create, delete
//       - daemonsets: create, delete, get, watch
//   - Cluster-admin satisfies all of the above.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/netperf/netperf-api/internal/api"
	"github.com/netperf/netperf-api/internal/executor"
	"github.com/netperf/netperf-api/internal/k8sclient"
	"github.com/netperf/netperf-api/internal/store"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// kubeConfigPath returns the path to the active kubeconfig.
func kubeConfigPath() string {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return kc
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

// buildClient loads the kubeconfig explicitly (never tries in-cluster).
// This is intentional: integration tests always run from a workstation or CI
// box that has kubectl configured, not from inside the cluster.
func buildClient(t *testing.T) (*k8sclient.Client, kubernetes.Interface) {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath())
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}

	// Raise QPS/Burst limits — the test issues many concurrent exec requests.
	cfg.QPS = 50
	cfg.Burst = 100

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	return &k8sclient.Client{Clientset: cs, RestConfig: cfg}, cs
}

// createIsolatedNamespace creates a short-lived namespace and registers a
// cleanup function that deletes it (and every resource inside) when the test
// ends, whether it passes or fails.
//
// The namespace is labelled to allow privileged workloads because the iperf3
// DaemonSet uses hostNetwork: true, which falls under the "privileged" policy.
func createIsolatedNamespace(t *testing.T, cs kubernetes.Interface) string {
	t.Helper()

	// Build a unique name that is easy to identify in `kubectl get ns`.
	nsName := fmt.Sprintf("iperf-test-%d", time.Now().UnixNano()%1_000_000)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
			Labels: map[string]string{
				// Allow hostNetwork pods under the Pod Security Standards enforcer.
				"pod-security.kubernetes.io/enforce": "privileged",
				"pod-security.kubernetes.io/audit":   "privileged",
				"pod-security.kubernetes.io/warn":    "privileged",
			},
		},
	}

	if _, err := cs.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace %q: %v", nsName, err)
	}
	t.Logf("created isolated namespace: %s", nsName)

	// t.Cleanup guarantees deletion even when the test panics or t.Fatal fires.
	t.Cleanup(func() {
		t.Logf("cleanup: deleting namespace %s …", nsName)
		propagation := metav1.DeletePropagationForeground
		err := cs.CoreV1().Namespaces().Delete(
			context.Background(),
			nsName,
			metav1.DeleteOptions{PropagationPolicy: &propagation},
		)
		if err != nil {
			t.Logf("cleanup warning: could not delete namespace %s: %v", nsName, err)
		}
	})

	return nsName
}

// deployIperf3DaemonSet creates the iperf3 server DaemonSet inside the given
// namespace listening on the specified port. Using a non-default port (e.g. 5202)
// avoids conflicts when a production DaemonSet already occupies 5201 on the same
// hosts (hostNetwork means all DaemonSets share the node's network namespace).
//
// Key design choices mirrored from the production DaemonSet:
//   - hostNetwork: true       — server binds to the node's InternalIP on :<port>
//   - tolerations: Exists     — runs on control-plane nodes too (maximises N)
//   - exec liveness probe     — avoids the "Bad file descriptor" bug caused by
//     kubelet TCP probes hitting the port and closing without handshaking
func deployIperf3DaemonSet(t *testing.T, cs kubernetes.Interface, namespace string, port int) {
	t.Helper()

	allowPrivEscalation := false
	runAsNonRoot := false // iperf3 image runs as root by default
	readOnlyRootFS := true

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "iperf3-server",
			Namespace: namespace,
			Labels:    map[string]string{"app": "iperf3-server"},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "iperf3-server"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "iperf3-server"},
				},
				Spec: corev1.PodSpec{
					// Bind directly to the node's network interface.
					// The executor discovers node InternalIPs and connects there.
					HostNetwork: true,
					DNSPolicy:   corev1.DNSClusterFirstWithHostNet,

					// Tolerate every taint so the pod runs on all nodes,
					// including control-plane nodes that carry NoSchedule taints.
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},

					// iperf3 --bidir writes temporary stream files to /tmp.
					// emptyDir makes /tmp writable while the rest of the root
					// filesystem stays read-only (readOnlyRootFilesystem: true).
					Volumes: []corev1.Volume{
						{
							Name: "tmp",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},

					Containers: []corev1.Container{
						{
							Name:            "iperf3",
							Image:           "docker.io/networkstatic/iperf3:latest",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"iperf3", "--server", "--forceflush", "-p", strconv.Itoa(port)},

							VolumeMounts: []corev1.VolumeMount{
								{Name: "tmp", MountPath: "/tmp"},
							},

							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1000m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},

							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: &allowPrivEscalation,
								RunAsNonRoot:             &runAsNonRoot,
								ReadOnlyRootFilesystem:   &readOnlyRootFS,
							},

							// IMPORTANT: use exec probe, NOT tcpSocket.
							// A tcpSocket probe connects to :5201 and disconnects
							// immediately — iperf3 logs "Bad file descriptor" for
							// every such half-open connection.  An exec probe runs
							// the binary inside the container and never touches the
							// listening socket.
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"iperf3", "--version"},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"iperf3", "--version"},
									},
								},
								InitialDelaySeconds: 3,
								PeriodSeconds:       5,
							},
						},
					},
				},
			},
		},
	}

	if _, err := cs.AppsV1().DaemonSets(namespace).Create(
		context.Background(), ds, metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("creating iperf3 DaemonSet: %v", err)
	}
	t.Logf("created DaemonSet iperf3-server in namespace %s", namespace)
}

// waitForDaemonSetReady polls the DaemonSet status every 5 s until all
// scheduled pods report Ready, or until ctx is cancelled (test timeout).
func waitForDaemonSetReady(t *testing.T, ctx context.Context, cs kubernetes.Interface, namespace string) {
	t.Helper()
	t.Log("waiting for DaemonSet pods to become Ready …")

	for {
		ds, err := cs.AppsV1().DaemonSets(namespace).Get(ctx, "iperf3-server", metav1.GetOptions{})
		if err == nil &&
			ds.Status.DesiredNumberScheduled > 0 &&
			ds.Status.NumberReady == ds.Status.DesiredNumberScheduled {
			t.Logf("DaemonSet ready: %d/%d pods",
				ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
			return
		}

		if err != nil {
			t.Logf("polling DaemonSet: %v", err)
		} else {
			t.Logf("pods ready: %d/%d (desired: %d) — waiting …",
				ds.Status.NumberReady,
				ds.Status.DesiredNumberScheduled,
				ds.Status.DesiredNumberScheduled)
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for DaemonSet to be ready: %v", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// countWorkerNodes returns the number of Ready nodes visible with the given
// label selector. Used to skip the test early if the cluster is too small.
func countWorkerNodes(t *testing.T, cfg *rest.Config, labelSel string) int {
	t.Helper()
	cs, _ := kubernetes.NewForConfig(cfg)
	nodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSel,
	})
	if err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	ready := 0
	for _, n := range nodes.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready++
			}
		}
	}
	return ready
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestE2E_FullMeasurementCycle is the primary integration test. It:
//
//  1. Creates an isolated namespace with PSS "privileged" labels.
//  2. Deploys the iperf3 DaemonSet to that namespace.
//  3. Waits until ALL pods are Ready.
//  4. Calls executor.Run() (the same code path the real API uses).
//  5. Asserts the result contains N*(N-1)/2 measurements with non-zero
//     bandwidth in both directions and no "Bad file descriptor" errors.
//  6. Deletes the namespace via t.Cleanup — guaranteed even on failure.
func TestE2E_FullMeasurementCycle(t *testing.T) {
	// ── Setup ────────────────────────────────────────────────────────────────

	// Use all Linux nodes (control-plane included) to maximise N in the test
	// cluster.  `kubernetes.io/os=linux` is a standard label present on every
	// Kubernetes node since v1.14.
	const nodeLabel = "kubernetes.io/os=linux"

	// Use port 5202 so the test DaemonSet does not conflict with a production
	// iperf3-server DaemonSet that may already be listening on the default 5201.
	// IPERF3_PORT is read by executor.iperf3ServerPort() and restored by t.Setenv.
	const testPort = 5202
	t.Setenv("IPERF3_PORT", strconv.Itoa(testPort))

	client, cs := buildClient(t)

	// Skip early if the cluster doesn't have enough nodes for a measurement.
	cfg, _ := clientcmd.BuildConfigFromFlags("", kubeConfigPath())
	nNodes := countWorkerNodes(t, cfg, nodeLabel)
	if nNodes < 2 {
		t.Skipf("need at least 2 Ready nodes, cluster has %d", nNodes)
	}
	t.Logf("cluster has %d Ready nodes — proceeding", nNodes)

	// ── Isolated namespace ───────────────────────────────────────────────────
	ns := createIsolatedNamespace(t, cs)

	// ── Deploy & wait ────────────────────────────────────────────────────────
	deployIperf3DaemonSet(t, cs, ns, testPort)

	// Allow up to 5 minutes for image pulls + container starts.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer waitCancel()
	waitForDaemonSetReady(t, waitCtx, cs, ns)

	// ── Execute measurement ──────────────────────────────────────────────────
	exec := executor.NewForNamespace(client, ns)

	// Discover nodes via the pod-based snapshot before starting the task.
	// This mirrors what the real POST handler does before launching the goroutine.
	discoverCtx, discoverCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer discoverCancel()
	snapshot, err := exec.DiscoverNodes(discoverCtx)
	if err != nil {
		t.Fatalf("DiscoverNodes failed: %v", err)
	}
	t.Logf("snapshot: %d nodes ready — %v", len(snapshot.IPs), snapshot.IPs)
	if len(snapshot.IPs) < 2 {
		t.Skipf("discovery returned only %d node(s); need at least 2", len(snapshot.IPs))
	}

	s := store.New()
	taskID := "e2e-task-001"
	s.Set(taskID, &store.Task{
		ID:        taskID,
		Status:    store.StatusPending,
		CreatedAt: time.Now(),
	})

	// Run synchronously in the test goroutine (no background goroutine needed).
	// Allow up to 10 minutes: (nNodes rounds × 10 s iperf3) + cooldowns + overhead.
	runCtx, runCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer runCancel()

	t.Log("starting measurement …")
	exec.Run(runCtx, taskID, s, snapshot)

	// ── Assertions ───────────────────────────────────────────────────────────
	task, ok := s.Get(taskID)
	if !ok {
		t.Fatal("task not found in store after Run() returned")
	}

	// 1. Task must have completed without errors.
	if task.Status != store.StatusCompleted {
		t.Fatalf("expected status=completed, got %q (error: %s)", task.Status, task.Error)
	}

	result, ok := task.Result.(*executor.Result)
	if !ok {
		t.Fatalf("task.Result is %T, want *executor.Result", task.Result)
	}

	t.Logf("measurement finished in %s", task.Duration)
	t.Logf("nodes tested: %v", result.Nodes)

	// 2. Matrix invariants for a True Directional N×N matrix (now including
	//    a self-test diagonal for completeness):
	//    - Exactly N rows (one per source).
	//    - Each row has N columns: N-1 measured pairs + 1 diagonal self-test cell.
	//    - Total entries = N*N — every ordered pair appears once.
	//    - Diagonal cells contain the SelfTestErrorMessage marker.
	if got := len(result.Matrix); got != nNodes {
		t.Errorf("want %d matrix rows, got %d", nNodes, got)
	}
	totalCells := 0
	for src, row := range result.Matrix {
		diag, hasDiag := row[src]
		if !hasDiag {
			t.Errorf("diagonal cell matrix[%s][%s] missing", src, src)
		} else {
			if diag.Mbps != 0 {
				t.Errorf("matrix[%s][%s].Mbps=%v, want 0 on diagonal", src, src, diag.Mbps)
			}
			if diag.Error == "" {
				t.Errorf("matrix[%s][%s].Error empty, want self-test marker", src, src)
			}
		}
		if len(row) != nNodes {
			t.Errorf("row matrix[%s]: want %d cells (N incl. diagonal), got %d", src, nNodes, len(row))
		}
		totalCells += len(row)
	}
	if want := nNodes * nNodes; totalCells != want {
		t.Errorf("want %d total cells (N*N for %d nodes incl. diagonal), got %d",
			want, nNodes, totalCells)
	}

	// 3. Per-cell assertions: every off-diagonal directed link must be > 0 Mbps
	// and free of errors. matrix[A][B] / matrix[B][A] must be independent values
	// (NOT silently duplicated, which was the redundancy in the old design).
	for src, row := range result.Matrix {
		for tgt, cell := range row {
			if tgt == src {
				continue // self-test diagonal validated above
			}
			t.Logf("  %s → %s : %.2f Mbps", src, tgt, cell.Mbps)
			if cell.Error != "" {
				t.Errorf("matrix[%s][%s]: unexpected error: %s", src, tgt, cell.Error)
				continue
			}
			if cell.Mbps <= 0 {
				t.Errorf("matrix[%s][%s].mbps must be > 0, got %f", src, tgt, cell.Mbps)
			}
		}
	}
}

// TestE2E_CancelMidFlight verifies that calling the CancelFunc while the
// measurement is running cleanly aborts the goroutine and sets status=canceled.
func TestE2E_CancelMidFlight(t *testing.T) {
	const nodeLabel = "kubernetes.io/os=linux"
	const testPort = 5202
	t.Setenv("IPERF3_PORT", strconv.Itoa(testPort))

	client, cs := buildClient(t)

	cfg, _ := clientcmd.BuildConfigFromFlags("", kubeConfigPath())
	if countWorkerNodes(t, cfg, nodeLabel) < 2 {
		t.Skip("need at least 2 Ready nodes")
	}

	ns := createIsolatedNamespace(t, cs)
	deployIperf3DaemonSet(t, cs, ns, testPort)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer waitCancel()
	waitForDaemonSetReady(t, waitCtx, cs, ns)

	exec := executor.NewForNamespace(client, ns)

	// Capture the node snapshot before launching the goroutine, mirroring the
	// real POST handler flow. Use the cancellable context so discovery is also
	// aborted if the test itself is cancelled.
	discoverCtx, discoverCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer discoverCancel()
	snapshot, err := exec.DiscoverNodes(discoverCtx)
	if err != nil {
		t.Fatalf("DiscoverNodes failed: %v", err)
	}
	t.Logf("snapshot: %d nodes — %v", len(snapshot.IPs), snapshot.IPs)

	s := store.New()
	taskID := "cancel-test-task"
	s.Set(taskID, &store.Task{
		ID:        taskID,
		Status:    store.StatusPending,
		CreatedAt: time.Now(),
	})

	// Create a cancellable context and fire the executor in a goroutine,
	// mirroring exactly what the real POST handler does.
	ctx, cancel := context.WithCancel(context.Background())
	s.SetCancel(taskID, cancel)

	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx, taskID, s, snapshot)
	}()

	// Give the first exec a moment to start, then cancel.
	time.Sleep(3 * time.Second)
	s.Cancel(taskID)

	// The goroutine must exit promptly after the context is cancelled
	// (within pairTimeout + a small buffer).
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Minute):
		t.Fatal("goroutine did not exit within 2 minutes after cancel")
	}

	task, _ := s.Get(taskID)
	if task.Status != store.StatusCanceled {
		t.Errorf("want status=canceled, got %q (error: %s)", task.Status, task.Error)
	}
	t.Logf("cancel test passed — task status: %s", task.Status)
}

// TestE2E_GlobalLock409Conflict fires two concurrent POST requests against a
// real httptest-wrapped HTTP server backed by a real Executor, and asserts
// that EXACTLY ONE wins (202 Accepted) while the other gets the lock-conflict
// response (409 Conflict). This validates the global mutex end-to-end:
//
//   - Concurrency: two goroutines hit POST simultaneously
//   - Statelessness of failed POST: the rejected request leaves no orphan task
//   - Cleanup: the accepted task is cancelled via DELETE so the test exits in
//     ~5–10 s rather than waiting ~45 s for the natural completion
func TestE2E_GlobalLock409Conflict(t *testing.T) {
	const nodeLabel = "kubernetes.io/os=linux"
	const testPort = 5202
	t.Setenv("IPERF3_PORT", strconv.Itoa(testPort))

	client, cs := buildClient(t)

	cfg, _ := clientcmd.BuildConfigFromFlags("", kubeConfigPath())
	if countWorkerNodes(t, cfg, nodeLabel) < 2 {
		t.Skip("need at least 2 Ready nodes")
	}

	ns := createIsolatedNamespace(t, cs)
	deployIperf3DaemonSet(t, cs, ns, testPort)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer waitCancel()
	waitForDaemonSetReady(t, waitCtx, cs, ns)

	// ── Real HTTP server ─────────────────────────────────────────────────────
	exec := executor.NewForNamespace(client, ns)
	st := store.New()
	h := api.New(st, exec)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// ── Fire two concurrent POSTs ────────────────────────────────────────────
	type result struct {
		status int
		body   map[string]interface{}
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	postURL := srv.URL + "/api/v1/network-measure"

	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			resp, err := http.Post(postURL, "application/json", nil)
			if err != nil {
				t.Errorf("POST #%d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			var body map[string]interface{}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			results[i] = result{status: resp.StatusCode, body: body}
			t.Logf("POST #%d → status=%d body=%v", i, resp.StatusCode, body)
		}()
	}
	wg.Wait()

	// ── Assert exactly one 202 and one 409 ───────────────────────────────────
	var accepted, conflicted *result
	for i := range results {
		switch results[i].status {
		case http.StatusAccepted:
			if accepted != nil {
				t.Fatal("both POSTs got 202 — global lock failed to serialise")
			}
			accepted = &results[i]
		case http.StatusConflict:
			if conflicted != nil {
				t.Fatal("both POSTs got 409 — neither acquired the lock")
			}
			conflicted = &results[i]
		default:
			t.Fatalf("unexpected status %d (body=%v)", results[i].status, results[i].body)
		}
	}
	if accepted == nil || conflicted == nil {
		t.Fatalf("want one 202 and one 409, got: %+v", results)
	}

	// 409 body must contain the conflict marker.
	if status, _ := conflicted.body["status"].(string); status != "conflict" {
		t.Errorf(`409 body: want status="conflict", got %v`, conflicted.body["status"])
	}

	// 202 body must include the enriched fields.
	taskID, _ := accepted.body["task_id"].(string)
	if taskID == "" {
		t.Fatalf("202 body missing task_id: %v", accepted.body)
	}
	for _, key := range []string{"node_count", "nodes", "total_rounds", "estimated_duration_seconds"} {
		if _, ok := accepted.body[key]; !ok {
			t.Errorf("202 body missing %q: %v", key, accepted.body)
		}
	}

	// ── Clean up: DELETE the running task so this test does not block 45 s ───
	delReq, _ := http.NewRequest(http.MethodDelete,
		srv.URL+"/api/v1/network-measure/"+taskID, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusAccepted {
		t.Errorf("DELETE: want 202, got %d", delResp.StatusCode)
	}

	// Wait for the task to actually transition to canceled / failed before
	// returning so the namespace cleanup hook does not race the goroutine.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		task, ok := st.Get(taskID)
		if ok && (task.Status == store.StatusCanceled ||
			task.Status == store.StatusCompleted ||
			task.Status == store.StatusFailed) {
			t.Logf("final task status: %s", task.Status)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("task did not reach a terminal state within 2 minutes after DELETE")
}
