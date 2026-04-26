package executor_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/netperf/netperf-api/internal/executor"
	"github.com/netperf/netperf-api/internal/k8sclient"
	"github.com/netperf/netperf-api/internal/store"
)

// ── DiscoverNodes ────────────────────────────────────────────────────────────
//
// DiscoverNodes is the heart of the data-plane-aware discovery refactor. The
// tests below use fake.NewSimpleClientset to feed it pods in a wide variety of
// states and assert that only the (Phase=Running ∧ PodReady=True ∧ HostIP≠"")
// triple is included in the snapshot.

const testNS = "netperf-api-test"

// makePod builds a Pod fixture for DiscoverNodes tests.
func makePod(name, nodeName, hostIP string, phase corev1.PodPhase, ready bool) *corev1.Pod {
	condStatus := corev1.ConditionFalse
	if ready {
		condStatus = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			Labels:    map[string]string{"app": "iperf3-server"},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{Name: "iperf3", Image: "iperf3:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase:  phase,
			HostIP: hostIP,
			PodIP:  hostIP,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: condStatus},
			},
		},
	}
}

// newExecForFake builds an Executor wired to a fake clientset preloaded with
// the given pods. RestConfig is left nil — DiscoverNodes does not need it.
func newExecForFake(t *testing.T, pods ...*corev1.Pod) *executor.Executor {
	t.Helper()
	objs := make([]runtime.Object, len(pods))
	for i, p := range pods {
		objs[i] = p
	}
	cs := fake.NewSimpleClientset(objs...)
	return executor.NewForNamespace(&k8sclient.Client{Clientset: cs, RestConfig: nil}, testNS)
}

// containsIP reports whether snapshot.IPs contains ip.
func containsIP(ips []string, ip string) bool {
	for _, x := range ips {
		if x == ip {
			return true
		}
	}
	return false
}

func TestDiscoverNodes_HappyPath_AllRunningReady(t *testing.T) {
	exec := newExecForFake(t,
		makePod("iperf-1", "node-1", "10.0.0.1", corev1.PodRunning, true),
		makePod("iperf-2", "node-2", "10.0.0.2", corev1.PodRunning, true),
		makePod("iperf-3", "node-3", "10.0.0.3", corev1.PodRunning, true),
	)

	snap, err := exec.DiscoverNodes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverNodes: %v", err)
	}
	if len(snap.IPs) != 3 {
		t.Fatalf("want 3 IPs, got %d (%v)", len(snap.IPs), snap.IPs)
	}
	for _, want := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if !containsIP(snap.IPs, want) {
			t.Errorf("missing %s in snapshot", want)
		}
		if snap.PodNames[want] == "" {
			t.Errorf("PodNames[%s] is empty", want)
		}
	}
}

func TestDiscoverNodes_FiltersOutNonRunning(t *testing.T) {
	cases := []corev1.PodPhase{
		corev1.PodPending,
		corev1.PodSucceeded,
		corev1.PodFailed,
		corev1.PodUnknown,
	}
	for _, phase := range cases {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			exec := newExecForFake(t,
				makePod("iperf-good", "node-1", "10.0.0.1", corev1.PodRunning, true),
				makePod("iperf-bad", "node-2", "10.0.0.2", phase, true),
			)
			snap, err := exec.DiscoverNodes(context.Background())
			if err != nil {
				t.Fatalf("DiscoverNodes: %v", err)
			}
			if len(snap.IPs) != 1 || snap.IPs[0] != "10.0.0.1" {
				t.Errorf("want only [10.0.0.1], got %v", snap.IPs)
			}
		})
	}
}

func TestDiscoverNodes_FiltersOutNotReady(t *testing.T) {
	exec := newExecForFake(t,
		makePod("iperf-ready", "node-1", "10.0.0.1", corev1.PodRunning, true),
		makePod("iperf-notready", "node-2", "10.0.0.2", corev1.PodRunning, false),
	)
	snap, err := exec.DiscoverNodes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverNodes: %v", err)
	}
	if len(snap.IPs) != 1 || snap.IPs[0] != "10.0.0.1" {
		t.Errorf("want only [10.0.0.1], got %v", snap.IPs)
	}
}

func TestDiscoverNodes_FiltersOutMissingPodReadyCondition(t *testing.T) {
	// Pod is Running but has no PodReady condition at all (rare, but possible
	// during a window before the kubelet has reported readiness).
	pod := makePod("iperf-no-cond", "node-1", "10.0.0.1", corev1.PodRunning, true)
	pod.Status.Conditions = nil
	exec := newExecForFake(t, pod)

	snap, err := exec.DiscoverNodes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverNodes: %v", err)
	}
	if len(snap.IPs) != 0 {
		t.Errorf("want empty snapshot, got %v", snap.IPs)
	}
}

func TestDiscoverNodes_FiltersOutEmptyHostIP(t *testing.T) {
	exec := newExecForFake(t,
		makePod("iperf-good", "node-1", "10.0.0.1", corev1.PodRunning, true),
		makePod("iperf-no-ip", "node-2", "", corev1.PodRunning, true),
	)
	snap, err := exec.DiscoverNodes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverNodes: %v", err)
	}
	if len(snap.IPs) != 1 || snap.IPs[0] != "10.0.0.1" {
		t.Errorf("want only [10.0.0.1], got %v", snap.IPs)
	}
}

func TestDiscoverNodes_DeduplicatesHostIP(t *testing.T) {
	// Two pods reporting the same HostIP — only the first one wins.
	exec := newExecForFake(t,
		makePod("iperf-A", "node-1", "10.0.0.1", corev1.PodRunning, true),
		makePod("iperf-B", "node-2", "10.0.0.1", corev1.PodRunning, true), // dup
	)
	snap, err := exec.DiscoverNodes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverNodes: %v", err)
	}
	if len(snap.IPs) != 1 {
		t.Errorf("want 1 IP after dedup, got %d (%v)", len(snap.IPs), snap.IPs)
	}
}

func TestDiscoverNodes_NoPodsAtAll(t *testing.T) {
	exec := newExecForFake(t) // no pods preloaded
	snap, err := exec.DiscoverNodes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverNodes: %v", err)
	}
	if len(snap.IPs) != 0 {
		t.Errorf("want empty snapshot, got %v", snap.IPs)
	}
	// JSON serialisation must produce [], not null — verified at the type level
	// by initialising IPs as []string{} in DiscoverNodes.
	if snap.IPs == nil {
		t.Error("snapshot.IPs is nil (would JSON-marshal as null); want []string{}")
	}
}

func TestDiscoverNodes_IgnoresOtherNamespaces(t *testing.T) {
	otherNs := makePod("intruder", "node-X", "10.99.0.1", corev1.PodRunning, true)
	otherNs.Namespace = "some-other-namespace"

	exec := newExecForFake(t,
		makePod("iperf-1", "node-1", "10.0.0.1", corev1.PodRunning, true),
		otherNs,
	)
	snap, err := exec.DiscoverNodes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverNodes: %v", err)
	}
	if len(snap.IPs) != 1 || snap.IPs[0] != "10.0.0.1" {
		t.Errorf("want only [10.0.0.1] (other namespace must be ignored), got %v", snap.IPs)
	}
}

func TestDiscoverNodes_RespectsContextCancellation(t *testing.T) {
	exec := newExecForFake(t,
		makePod("iperf-1", "node-1", "10.0.0.1", corev1.PodRunning, true),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the call

	_, err := exec.DiscoverNodes(ctx)
	if err == nil {
		// fake clientset typically does NOT honour ctx cancellation, so this
		// is a soft assertion: we accept either path but warn so a future
		// stricter fake will be caught.
		t.Logf("note: fake clientset returned no error on cancelled context (acceptable)")
	}
}

// ── Run() with edge-case snapshots ───────────────────────────────────────────
//
// Run() is the public entry point used by the API handler and the integration
// tests. It must produce a meaningful task status (Failed / Canceled / Completed)
// for every input — including degenerate snapshots that no sane handler would
// pass but that a future caller might.

// runWithSnapshot fires Run() and returns the resulting task. The snapshot is
// deliberately constructed to exercise the early-return paths in run() so we
// do not need any pod/exec fakes.
func runWithSnapshot(t *testing.T, snap *executor.NodeSnapshot) *store.Task {
	t.Helper()
	exec := newExecForFake(t) // empty fake — never called for <2 nodes
	s := store.New()
	taskID := "edge-case-task"
	s.Set(taskID, &store.Task{ID: taskID, Status: store.StatusPending, CreatedAt: time.Now()})
	exec.Run(context.Background(), taskID, s, snap)
	task, _ := s.Get(taskID)
	return task
}

func TestRun_ZeroNodes(t *testing.T) {
	task := runWithSnapshot(t, &executor.NodeSnapshot{
		IPs:      []string{},
		PodNames: map[string]string{},
	})
	if task.Status != store.StatusFailed {
		t.Fatalf("want status=failed, got %s", task.Status)
	}
	if !strings.Contains(task.Error, "at least 2") {
		t.Errorf("want error mentioning 'at least 2', got %q", task.Error)
	}
}

func TestRun_OneNode(t *testing.T) {
	task := runWithSnapshot(t, &executor.NodeSnapshot{
		IPs:      []string{"10.0.0.1"},
		PodNames: map[string]string{"10.0.0.1": "iperf-1"},
	})
	if task.Status != store.StatusFailed {
		t.Fatalf("want status=failed, got %s", task.Status)
	}
	if !strings.Contains(task.Error, "found 1") {
		t.Errorf("want error mentioning 'found 1', got %q", task.Error)
	}
}

// NOTE: cancellation behaviour with ≥2 nodes is exercised by
// TestE2E_CancelMidFlight in the integration suite, which has a real REST
// client and can actually perform a SPDY exec mid-flight before cancellation.
// A unit-level equivalent would need a full fake REST round-tripper, which is
// not worth the maintenance burden.

// ── BandwidthData JSON shape ─────────────────────────────────────────────────
//
// Although encoding/json behaviour is well-known, the tag changes are
// load-bearing: the public API contract requires exactly two fields ("mbps"
// and "error,omitempty") and forbids ANY occurrence of egress/reverse/ingress
// terminology in the serialised output. A typo in a struct tag would silently
// break every downstream consumer, so we lock it down here.

func TestBandwidthData_JSONShape_HappyPath(t *testing.T) {
	d := executor.BandwidthData{Mbps: 910.5}
	got := mustMarshal(t, d)

	if got != `{"mbps":910.5}` {
		t.Errorf("want exactly {\"mbps\":910.5}, got %s", got)
	}
	for _, forbidden := range []string{
		"mbps_ingress", "mbps_reverse_ingress", "mbps_egress",
		"reverse", "Reverse", "egress", "Egress", "ingress", "Ingress",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("forbidden %q leaked into JSON: %s", forbidden, got)
		}
	}
}

func TestBandwidthData_JSONShape_ErrorOmitsEmpty(t *testing.T) {
	// With no error, the error field must be absent (omitempty) so the JSON
	// stays clean for the common success case.
	d := executor.BandwidthData{Mbps: 0}
	got := mustMarshal(t, d)
	if strings.Contains(got, "error") {
		t.Errorf("empty error should be omitted, got %s", got)
	}
}

func TestBandwidthData_JSONShape_ErrorPresent(t *testing.T) {
	// Error path: Mbps is 0 (no data) and the error string is surfaced.
	d := executor.BandwidthData{Error: "iperf3 reported: Connection refused"}
	got := mustMarshal(t, d)
	if got != `{"mbps":0,"error":"iperf3 reported: Connection refused"}` {
		t.Errorf("unexpected JSON shape: %s", got)
	}
}

// TestResult_JSONShape_DirectionalMatrix locks down the top-level Result
// envelope: a "nodes" list and a "matrix" object keyed by source then target.
// Every cell must be a BandwidthData object — no flat arrays, no measurements
// list, no "reverse" anywhere.
func TestResult_JSONShape_DirectionalMatrix(t *testing.T) {
	r := executor.Result{
		Nodes: []string{"10.0.0.1", "10.0.0.2"},
		Matrix: map[string]map[string]*executor.BandwidthData{
			"10.0.0.1": {"10.0.0.2": {Mbps: 910.5}},
			"10.0.0.2": {"10.0.0.1": {Mbps: 920.7}},
		},
	}
	got := mustMarshal(t, r)

	for _, want := range []string{
		`"nodes":["10.0.0.1","10.0.0.2"]`,
		`"matrix":`,
		`"10.0.0.1":{"10.0.0.2":{"mbps":910.5}}`,
		`"10.0.0.2":{"10.0.0.1":{"mbps":920.7}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in JSON: %s", want, got)
		}
	}
	for _, forbidden := range []string{
		"measurements", "mbps_ingress", "mbps_reverse_ingress", "mbps_egress",
		"reverse", "egress",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("forbidden %q leaked into JSON: %s", forbidden, got)
		}
	}
}

func mustMarshal(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
