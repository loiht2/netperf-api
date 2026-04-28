package executor_test

// This file is the QA-grade unit-test suite for the Store-and-Fetch execution
// architecture introduced in commit 6b8da05.
//
// It cannot live in executor_test.go because that file's helpers were written
// for the older single-exec architecture. Splitting the new tests off keeps
// each file focused and makes the QA coverage easy to audit at a glance.
//
// The suite uses a fakeExec harness that intercepts every podExec call,
// classifies it as Step A / Step B / Step C, records it for later inspection,
// and dispatches to a per-test handler. This decouples test scenarios from
// the real SPDY remotecommand layer so we can drive every path of execPair
// deterministically — Tailscale flaps, fetch retries, parse failures,
// cancellation mid-flight, and concurrent N-pair rounds — in milliseconds.
//
// Production timing constants are scaled down via SetTimingForTesting so the
// full test file runs in under five seconds with the -race detector enabled.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/netperf/netperf-api/internal/executor"
	"github.com/netperf/netperf-api/internal/k8sclient"
	"github.com/netperf/netperf-api/internal/store"
)

// ── Step classification ──────────────────────────────────────────────────────

type stepKind int

const (
	stepUnknown stepKind = iota
	stepA                // sh -c "iperf3 ... > /tmp/file"
	stepB                // cat /tmp/file
	stepC                // rm -f /tmp/file
)

func (k stepKind) String() string {
	switch k {
	case stepA:
		return "A"
	case stepB:
		return "B"
	case stepC:
		return "C"
	}
	return "?"
}

// classifyCmd inspects argv[0] to bucket each podExec call.
func classifyCmd(cmd []string) stepKind {
	if len(cmd) == 0 {
		return stepUnknown
	}
	switch cmd[0] {
	case "sh":
		return stepA
	case "cat":
		return stepB
	case "rm":
		return stepC
	}
	return stepUnknown
}

// ── Fake exec harness ────────────────────────────────────────────────────────

// fakeCall captures one invocation of the fake execFunc.
type fakeCall struct {
	Idx  int      // sequential index across the entire test
	Pod  string   // pod name passed to podExec
	Cmd  []string // command argv
	Kind stepKind // classified Step A/B/C/Unknown
	When time.Time
}

// fakeExec is a deterministic, thread-safe replacement for the SPDY exec layer.
//
// Each call is recorded under a mutex so concurrent goroutines (multiple pairs
// in a round, multiple rounds, etc.) cannot race the call log.  The user
// supplies a `handler` function that decides what to return per call — by
// classifying via `call.Kind` and/or counting per-kind calls atomically.
type fakeExec struct {
	mu      sync.Mutex
	calls   []fakeCall
	handler func(call fakeCall) (string, string, error)
}

func (f *fakeExec) exec(_ context.Context, pod string, cmd []string) (string, string, error) {
	f.mu.Lock()
	call := fakeCall{
		Idx:  len(f.calls),
		Pod:  pod,
		Cmd:  append([]string(nil), cmd...),
		Kind: classifyCmd(cmd),
		When: time.Now(),
	}
	f.calls = append(f.calls, call)
	h := f.handler
	f.mu.Unlock()

	if h == nil {
		return "", "", nil
	}
	return h(call)
}

func (f *fakeExec) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeExec) callsByKind(k stepKind) []fakeCall {
	out := []fakeCall{}
	for _, c := range f.snapshot() {
		if c.Kind == k {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeExec) countByKind(k stepKind) int {
	return len(f.callsByKind(k))
}

// ── Canonical iperf3 --bidir JSON fixtures ───────────────────────────────────

// validBidirJSON returns an iperf3 -J document with the supplied bps values.
// Includes mixed-type fields ("sender": true/false, "retransmits": int) to
// exercise the parser's tolerance for non-numeric siblings — see Bug 3 sub-B.
func validBidirJSON(forwardBps, reverseBps float64) string {
	payload := map[string]any{
		"start": map[string]any{
			"test_start": map[string]any{"duration": 10},
		},
		"end": map[string]any{
			"sum_received": map[string]any{
				"bits_per_second": forwardBps,
				"retransmits":     0,
				"sender":          true,
			},
			"sum_received_bidir_reverse": map[string]any{
				"bits_per_second": reverseBps,
				"retransmits":     0,
				"sender":          false,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// iperf3ConnectionRefusedJSON is what the iperf3 client emits when the server
// is unreachable: a top-level "error" field, no "end" object.
const iperf3ConnectionRefusedJSON = `{"error": "unable to connect to server: Connection refused"}`

// ── Common helpers ───────────────────────────────────────────────────────────

// newFakeExecutor returns an Executor wired to a noop fake clientset and a
// programmable fakeExec. Timing overrides are applied so the test runs in the
// 10s-of-milliseconds range, never seconds.
func newFakeExecutor(t *testing.T) (*executor.Executor, *fakeExec) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	e := executor.NewForNamespace(&k8sclient.Client{Clientset: cs, RestConfig: nil}, "netperf-api-test")
	fe := &fakeExec{}
	e.SetExecFunc(fe.exec)
	// fileWait=50ms keeps Step A's mandatory wait short.
	// fetchRetryWait=10ms makes the 4-attempt fetch loop take ~30ms in the worst case.
	// roundCooldown=20ms keeps multi-round tests fast.
	e.SetTimingForTesting(50*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond)
	return e, fe
}

// snap builds a deterministic NodeSnapshot in the order given.
func snap(ips ...string) *executor.NodeSnapshot {
	s := &executor.NodeSnapshot{
		IPs:      append([]string(nil), ips...),
		PodNames: make(map[string]string, len(ips)),
	}
	for _, ip := range ips {
		s.PodNames[ip] = "iperf-" + strings.ReplaceAll(ip, ".", "-")
	}
	return s
}

// runOnce drives Run() to completion against a freshly-created task and
// returns the resulting Task.  Synchronous: returns when Run() returns.
func runOnce(t *testing.T, e *executor.Executor, s *executor.NodeSnapshot, taskID string) *store.Task {
	t.Helper()
	st := store.New()
	st.Set(taskID, &store.Task{ID: taskID, Status: store.StatusPending, CreatedAt: time.Now()})
	e.Run(context.Background(), taskID, st, s)
	tk, _ := st.Get(taskID)
	return tk
}

// runOnceCtx is like runOnce but lets the caller supply the context (for
// cancellation tests).
func runOnceCtx(t *testing.T, ctx context.Context, e *executor.Executor, s *executor.NodeSnapshot, taskID string) *store.Task {
	t.Helper()
	st := store.New()
	st.Set(taskID, &store.Task{ID: taskID, Status: store.StatusPending, CreatedAt: time.Now()})
	e.Run(ctx, taskID, st, s)
	tk, _ := st.Get(taskID)
	return tk
}

// withinPct asserts that got is within +/- pct of want.
func withinPct(got, want, pct float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff <= want*pct/100.0
}

// ════════════════════════════════════════════════════════════════════════════
// 1. Store-and-Fetch flow tests — Happy Path / Retry Rescue / Ultimate Failure
// ════════════════════════════════════════════════════════════════════════════

// TestStoreAndFetch_HappyPath: Step A succeeds → Step B succeeds first try →
// Step C cleans up. Asserts the matrix has correct directional values and the
// 3-call sequence is exactly (A, B, C) for one pair.
func TestStoreAndFetch_HappyPath(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		switch c.Kind {
		case stepA:
			return "", "", nil
		case stepB:
			return validBidirJSON(910e6, 920e6), "", nil
		case stepC:
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected cmd: %v", c.Cmd)
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "happy")

	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s, want completed (err=%s)", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)

	cellAB := res.Matrix["10.0.0.1"]["10.0.0.2"]
	cellBA := res.Matrix["10.0.0.2"]["10.0.0.1"]
	if cellAB == nil || cellBA == nil {
		t.Fatalf("missing matrix cells; matrix=%+v", res.Matrix)
	}
	if !withinPct(cellAB.Mbps, 910, 0.5) {
		t.Errorf("matrix[A][B].Mbps=%v, want ~910", cellAB.Mbps)
	}
	if !withinPct(cellBA.Mbps, 920, 0.5) {
		t.Errorf("matrix[B][A].Mbps=%v, want ~920", cellBA.Mbps)
	}
	if cellAB.Error != "" || cellBA.Error != "" {
		t.Errorf("unexpected errors: AB=%q BA=%q", cellAB.Error, cellBA.Error)
	}

	// Exactly one of each step.
	if got := fe.countByKind(stepA); got != 1 {
		t.Errorf("Step A calls=%d, want 1", got)
	}
	if got := fe.countByKind(stepB); got != 1 {
		t.Errorf("Step B calls=%d, want 1", got)
	}
	if got := fe.countByKind(stepC); got != 1 {
		t.Errorf("Step C calls=%d, want 1", got)
	}

	// Order must be A, then B, then C — never C before B.
	calls := fe.snapshot()
	if !(calls[0].Kind == stepA && calls[1].Kind == stepB && calls[2].Kind == stepC) {
		t.Errorf("expected order A,B,C; got %s,%s,%s", calls[0].Kind, calls[1].Kind, calls[2].Kind)
	}
}

// TestStoreAndFetch_RetryRescue: Step B fails twice (empty stdout, simulating
// a cat stream dropped mid-fetch) then succeeds on the 3rd try. Matrix must
// contain the parsed values from the 3rd response and Step C still runs once.
func TestStoreAndFetch_RetryRescue_SucceedsOnThirdAttempt(t *testing.T) {
	e, fe := newFakeExecutor(t)
	var fetchCount atomic.Int32
	fe.handler = func(c fakeCall) (string, string, error) {
		switch c.Kind {
		case stepA:
			return "", "", nil
		case stepB:
			n := fetchCount.Add(1)
			if n < 3 {
				// First two attempts: empty stdout (cat stream dropped).
				return "", "", nil
			}
			return validBidirJSON(800e6, 850e6), "", nil
		case stepC:
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected: %v", c.Cmd)
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "rescue")

	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s, want completed (err=%s)", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)
	got := res.Matrix["10.0.0.1"]["10.0.0.2"].Mbps
	if !withinPct(got, 800, 0.5) {
		t.Errorf("matrix[A][B].Mbps=%v, want ~800 (third-attempt response)", got)
	}

	if got := fe.countByKind(stepB); got != 3 {
		t.Errorf("Step B attempts=%d, want exactly 3 (2 failures + 1 success)", got)
	}
	if got := fe.countByKind(stepC); got != 1 {
		t.Errorf("Step C calls=%d, want exactly 1", got)
	}
}

// TestStoreAndFetch_AllFetchAttemptsFail: every fetch returns empty.  All four
// attempts (initial + 3 retries) are exhausted; the matrix cell carries a
// descriptive error and the Go struct Mbps field is 0 (serialised as null in
// JSON — verified below).  Step C still runs.
func TestStoreAndFetch_AllFetchAttemptsFail(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		// Step A and Step C succeed; Step B always returns empty.
		return "", "", nil
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "ultimate-failure")

	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s, want completed (matrix should still be returned with error cells)", task.Status)
	}
	res := task.Result.(*executor.Result)

	for _, ips := range [][2]string{{"10.0.0.1", "10.0.0.2"}, {"10.0.0.2", "10.0.0.1"}} {
		cell := res.Matrix[ips[0]][ips[1]]
		if cell == nil {
			t.Fatalf("matrix[%s][%s] missing", ips[0], ips[1])
		}
		// Internal Go value is 0 (float64 zero-value on the struct).
		if cell.Mbps != 0 {
			t.Errorf("matrix[%s][%s].Mbps (struct)=%v, want 0", ips[0], ips[1], cell.Mbps)
		}
		if cell.Error == "" {
			t.Errorf("matrix[%s][%s].Error empty, want a descriptive failure", ips[0], ips[1])
		}
		if !strings.Contains(cell.Error, "fetch") {
			t.Errorf("matrix[%s][%s].Error=%q, want it to mention fetch attempts", ips[0], ips[1], cell.Error)
		}
		// JSON output must be {"mbps":null,"error":"..."} — not {"mbps":0,...}.
		b, err := json.Marshal(cell)
		if err != nil {
			t.Fatalf("marshal cell: %v", err)
		}
		js := string(b)
		if !strings.Contains(js, `"mbps":null`) {
			t.Errorf("matrix[%s][%s] JSON: want mbps:null, got %s", ips[0], ips[1], js)
		}
		if strings.Contains(js, `"mbps":0`) {
			t.Errorf("matrix[%s][%s] JSON: must not contain mbps:0, got %s", ips[0], ips[1], js)
		}
	}

	if got := fe.countByKind(stepB); got != 4 {
		t.Errorf("Step B attempts=%d, want 4 (initial + 3 retries)", got)
	}
	if got := fe.countByKind(stepC); got != 1 {
		t.Errorf("Step C calls=%d, want exactly 1 (always cleanup)", got)
	}
}

// TestStoreAndFetch_CleanupAlwaysCalled: table-driven proof that Step C runs
// for every failure mode.  This is the load-bearing guarantee of the deferred
// cleanup — without it, a retry-storm could fill /tmp and DoS the pod.
func TestStoreAndFetch_CleanupAlwaysCalled(t *testing.T) {
	tests := []struct {
		name    string
		handler func(c fakeCall) (string, string, error)
	}{
		{
			name: "step_a_returns_error_immediately",
			handler: func(c fakeCall) (string, string, error) {
				if c.Kind == stepA {
					return "", "", errors.New("SPDY stream broken")
				}
				if c.Kind == stepB {
					return validBidirJSON(900e6, 905e6), "", nil
				}
				return "", "", nil
			},
		},
		{
			name: "step_b_returns_garbage_json",
			handler: func(c fakeCall) (string, string, error) {
				switch c.Kind {
				case stepA:
					return "", "", nil
				case stepB:
					return "this is not json at all <html><body>", "", nil
				}
				return "", "", nil
			},
		},
		{
			name: "step_b_returns_iperf3_application_error",
			handler: func(c fakeCall) (string, string, error) {
				switch c.Kind {
				case stepA:
					return "", "", nil
				case stepB:
					return iperf3ConnectionRefusedJSON, "", nil
				}
				return "", "", nil
			},
		},
		{
			name: "step_b_returns_truncated_json",
			handler: func(c fakeCall) (string, string, error) {
				switch c.Kind {
				case stepA:
					return "", "", nil
				case stepB:
					return `{"start":{"test_start":{"duration":10`, "", nil
				}
				return "", "", nil
			},
		},
		{
			name: "step_b_succeeds_but_step_c_fails_dont_propagate",
			handler: func(c fakeCall) (string, string, error) {
				switch c.Kind {
				case stepA:
					return "", "", nil
				case stepB:
					return validBidirJSON(900e6, 905e6), "", nil
				case stepC:
					return "", "rm: cannot remove", errors.New("rm failed")
				}
				return "", "", nil
			},
		},
		{
			name:    "all_attempts_empty",
			handler: func(c fakeCall) (string, string, error) { return "", "", nil },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fe := newFakeExecutor(t)
			fe.handler = tc.handler

			runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "cleanup-"+tc.name)

			if got := fe.countByKind(stepC); got != 1 {
				t.Errorf("Step C count=%d, want exactly 1 (cleanup must always run)", got)
			}
			// Step C must always be the LAST call for the pair.
			calls := fe.snapshot()
			if calls[len(calls)-1].Kind != stepC {
				t.Errorf("last call=%s, want stepC (cleanup must be the final call)", calls[len(calls)-1].Kind)
			}
		})
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 2. Matrix correctness — directional mapping, diagonal, no egress leaks
// ════════════════════════════════════════════════════════════════════════════

// TestRun_DirectionalMapping_ForwardAndReverseAreIndependent: feed clearly
// distinct forward and reverse bps; assert each populates its own cell with
// no cross-contamination.
func TestRun_DirectionalMapping_ForwardAndReverseAreIndependent(t *testing.T) {
	e, fe := newFakeExecutor(t)
	const forwardBps = 723e6 // unique sentinel — must end up in matrix[A][B]
	const reverseBps = 451e6 // unique sentinel — must end up in matrix[B][A]
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(forwardBps, reverseBps), "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "direction")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)

	if got := res.Matrix["10.0.0.1"]["10.0.0.2"].Mbps; !withinPct(got, 723, 0.5) {
		t.Errorf("matrix[A][B].Mbps=%v, want ~723 (forward sentinel)", got)
	}
	if got := res.Matrix["10.0.0.2"]["10.0.0.1"].Mbps; !withinPct(got, 451, 0.5) {
		t.Errorf("matrix[B][A].Mbps=%v, want ~451 (reverse sentinel)", got)
	}
}

// TestRun_NoDiagonalInMatrix: matrix[X][X] must be absent for every node.
// The diagonal was removed — self-test entries must never appear in the output.
func TestRun_NoDiagonalInMatrix(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(900e6, 905e6), "", nil
		}
		return "", "", nil
	}

	nodes := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	task := runOnce(t, e, snap(nodes...), "no-diagonal")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)

	for _, ip := range nodes {
		if cell, exists := res.Matrix[ip][ip]; exists {
			t.Errorf("matrix[%s][%s] must be absent (no diagonal), got %+v", ip, ip, cell)
		}
	}
}

// TestRun_MatrixShapeNxNMinus1: total cells = N*(N-1). Each row has exactly
// N-1 entries (all peers except self). No diagonal entry is present.
func TestRun_MatrixShapeNxNMinus1(t *testing.T) {
	tests := []struct {
		name string
		n    int
	}{
		{"two_nodes", 2},
		{"three_nodes_odd", 3},
		{"four_nodes_even", 4},
		{"five_nodes_odd", 5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ips := make([]string, tc.n)
			for i := 0; i < tc.n; i++ {
				ips[i] = fmt.Sprintf("10.0.0.%d", i+1)
			}

			e, fe := newFakeExecutor(t)
			fe.handler = func(c fakeCall) (string, string, error) {
				if c.Kind == stepB {
					return validBidirJSON(900e6, 905e6), "", nil
				}
				return "", "", nil
			}

			task := runOnce(t, e, snap(ips...), "matrix-shape-"+tc.name)
			if task.Status != store.StatusCompleted {
				t.Fatalf("status=%s err=%s", task.Status, task.Error)
			}
			res := task.Result.(*executor.Result)

			if got := len(res.Matrix); got != tc.n {
				t.Errorf("rows=%d, want %d", got, tc.n)
			}
			wantPerRow := tc.n - 1
			totalCells := 0
			for src, row := range res.Matrix {
				if cell, exists := row[src]; exists {
					t.Errorf("matrix[%s][%s] must be absent (diagonal forbidden), got %+v", src, src, cell)
				}
				if len(row) != wantPerRow {
					t.Errorf("matrix[%s] has %d cells, want %d (N-1, no diagonal)", src, len(row), wantPerRow)
				}
				totalCells += len(row)
			}
			if want := tc.n * (tc.n - 1); totalCells != want {
				t.Errorf("total cells=%d, want N*(N-1)=%d for N=%d", totalCells, want, tc.n)
			}
		})
	}
}

// TestRun_NoEgressFieldsLeak_InMatrixJSON: the entire serialised payload must
// not contain "egress", "reverse", or any sender-side terminology.
func TestRun_NoEgressFieldsLeak_InMatrixJSON(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(900e6, 905e6), "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "no-egress")
	res := task.Result.(*executor.Result)

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, forbidden := range []string{
		"egress", "Egress", "ingress", "Ingress",
		"reverse", "Reverse", "sender", "Sender",
		"mbps_egress", "mbps_reverse_ingress", "fwd_sent", "rev_sent",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("forbidden token %q leaked into JSON: %s", forbidden, got)
		}
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 3. Concurrency — race detector under heavy parallel load
// ════════════════════════════════════════════════════════════════════════════

// TestRun_ConcurrentRounds_HeavyLoad: drives Run() with N=8 (odd-padding kicks
// in for one schedule entry) so the executor launches up to 4 concurrent pair
// goroutines per round across 8 rounds (or 7 for even N=8 — circle method).
// All call recordings, matrix writes, and goroutine accounting must be
// race-free under -race.
func TestRun_ConcurrentRounds_HeavyLoad_RaceFree(t *testing.T) {
	const N = 8
	ips := make([]string, N)
	for i := 0; i < N; i++ {
		ips[i] = fmt.Sprintf("10.10.0.%d", i+1)
	}

	e, fe := newFakeExecutor(t)
	// Use unique forward/reverse bps per call so any cross-contamination (which
	// would mean a real data race) is detectable.
	var stepBCount atomic.Int64
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			n := stepBCount.Add(1)
			return validBidirJSON(float64(900+n)*1e6, float64(800+n)*1e6), "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap(ips...), "concurrent")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)

	// Every off-diagonal cell must be populated and bandwidth > 0.
	for src, row := range res.Matrix {
		for tgt, cell := range row {
			if tgt == src {
				continue
			}
			if cell == nil {
				t.Errorf("matrix[%s][%s] missing", src, tgt)
				continue
			}
			if cell.Error != "" {
				t.Errorf("matrix[%s][%s] error=%q", src, tgt, cell.Error)
			}
			if cell.Mbps <= 0 {
				t.Errorf("matrix[%s][%s].Mbps=%v, want >0", src, tgt, cell.Mbps)
			}
		}
	}

	// Step counts: N*(N-1)/2 = 28 pairs total, each with 1×Step A + 1×Step B + 1×Step C.
	const expectedPairs = N * (N - 1) / 2
	if got := fe.countByKind(stepA); got != expectedPairs {
		t.Errorf("Step A=%d, want %d pairs", got, expectedPairs)
	}
	if got := fe.countByKind(stepB); got != expectedPairs {
		t.Errorf("Step B=%d, want %d pairs", got, expectedPairs)
	}
	if got := fe.countByKind(stepC); got != expectedPairs {
		t.Errorf("Step C=%d, want %d pairs", got, expectedPairs)
	}
}

// TestRun_PartialFailures_OnePairBreaksRoundContinues: in a 4-node measurement
// with two pairs per round, one pair's fetch fails persistently while the
// other succeeds.  Every round must still complete, the failing pair's cells
// hold the error, and the surviving pair's cells hold valid bandwidth.
func TestRun_PartialFailures_OnePairBreaksRoundContinues(t *testing.T) {
	const failTarget = "10.0.0.4" // any pair touching this IP fails Step B

	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		switch c.Kind {
		case stepA:
			return "", "", nil
		case stepB:
			// Step B's cmd is ["cat", "/tmp/iperf_<task>_R<n>_<src>_<dst>.json"]
			if len(c.Cmd) >= 2 && strings.Contains(c.Cmd[1], failTarget) {
				return "", "", nil // empty → exhaust retries
			}
			return validBidirJSON(800e6, 850e6), "", nil
		case stepC:
			return "", "", nil
		}
		return "", "", fmt.Errorf("unexpected: %v", c.Cmd)
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"), "partial")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)

	// Cells touching .4 must be errored, others OK.
	for src, row := range res.Matrix {
		for tgt, cell := range row {
			if tgt == src {
				continue
			}
			involvesFail := src == failTarget || tgt == failTarget
			if involvesFail {
				if cell.Error == "" {
					t.Errorf("matrix[%s][%s] should be errored (touches %s)", src, tgt, failTarget)
				}
				if cell.Mbps != 0 {
					t.Errorf("matrix[%s][%s].Mbps (struct)=%v, want 0 on failure (JSON will be null)", src, tgt, cell.Mbps)
				}
			} else {
				if cell.Error != "" {
					t.Errorf("matrix[%s][%s] unexpectedly errored: %s", src, tgt, cell.Error)
				}
				if cell.Mbps <= 0 {
					t.Errorf("matrix[%s][%s].Mbps=%v, want >0", src, tgt, cell.Mbps)
				}
			}
		}
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 4. Edge cases — degenerate snapshots, corrupt inputs, cancellations
// ════════════════════════════════════════════════════════════════════════════

// TestExecPair_PodNotInSnapshot: the source IP is not in the PodNames map.
// The pair must error immediately without invoking ANY exec call (no Step A,
// no Step B, no Step C — there is no pod to clean up against).
func TestExecPair_PodNotInSnapshot_NoExecCalls(t *testing.T) {
	e, fe := newFakeExecutor(t)
	// Manually construct a snapshot where one IP is missing from PodNames.
	s := &executor.NodeSnapshot{
		IPs: []string{"10.0.0.1", "10.0.0.2"},
		PodNames: map[string]string{
			// "10.0.0.1" deliberately omitted.
			"10.0.0.2": "iperf-2",
		},
	}
	fe.handler = func(c fakeCall) (string, string, error) {
		// If anything is called we want to know which step.
		t.Logf("unexpected exec call: kind=%s cmd=%v", c.Kind, c.Cmd)
		return "", "", nil
	}

	task := runOnce(t, e, s, "no-pod")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)
	cell := res.Matrix["10.0.0.1"]["10.0.0.2"]
	if cell == nil || cell.Error == "" {
		t.Errorf("expected error cell for pair with missing pod; got %+v", cell)
	}
	if !strings.Contains(cell.Error, "no running iperf3 pod") {
		t.Errorf("error=%q, want it to mention missing pod", cell.Error)
	}
	// No exec calls should have happened for this pair.
	if got := len(fe.snapshot()); got != 0 {
		t.Errorf("got %d exec calls, want 0 (early return before any exec)", got)
	}
}

// TestStoreAndFetch_CorruptOrInvalidOutput: table-driven attack on the parser
// via Step B.  Each row supplies a malicious / malformed Step B response and
// asserts that the executor:
//   - returns task status=completed (does not crash, does not return Failed)
//   - the matrix cell has an internal zero value and a non-empty error
//     (API JSON renders mbps as null)
//   - Step C cleanup still runs
func TestStoreAndFetch_CorruptOrInvalidOutput(t *testing.T) {
	tests := []struct {
		name   string
		stepB  string
		stepBE error
		// On Mbps assertion: most cases expect Mbps=0; for "iperf3 reports zero"
		// we expect Mbps=0 too but with a different error semantics.
		wantErrSubstr string
	}{
		{name: "garbage_html", stepB: `<html>500 Internal Server Error</html>`, wantErrSubstr: "parse"},
		{name: "binary_noise", stepB: "\x00\x01\x02\x03\x04\xff\xfe", wantErrSubstr: "parse"},
		{name: "truncated_mid_object", stepB: `{"start":{"test_start":{"duration":10`, wantErrSubstr: "parse"},
		{name: "iperf3_connection_refused", stepB: iperf3ConnectionRefusedJSON, wantErrSubstr: "iperf3"},
		{name: "valid_envelope_missing_end", stepB: `{"start":{"test_start":{"duration":10}}}`, wantErrSubstr: ""},
		{name: "huge_garbage_first_then_json", stepB: strings.Repeat("garbage line\n", 200) + validBidirJSON(900e6, 905e6), wantErrSubstr: ""},
		// stderr-ish leakage but no JSON at all
		{name: "only_iperf3_stderr_text", stepB: "iperf3: error - unable to connect: Connection refused", wantErrSubstr: "parse"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fe := newFakeExecutor(t)
			fe.handler = func(c fakeCall) (string, string, error) {
				switch c.Kind {
				case stepA:
					return "", "", nil
				case stepB:
					return tc.stepB, "", tc.stepBE
				case stepC:
					return "", "", nil
				}
				return "", "", fmt.Errorf("unexpected")
			}

			task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "corrupt-"+tc.name)
			if task.Status != store.StatusCompleted {
				t.Fatalf("status=%s err=%s", task.Status, task.Error)
			}
			res := task.Result.(*executor.Result)
			cell := res.Matrix["10.0.0.1"]["10.0.0.2"]
			if cell == nil {
				t.Fatalf("matrix cell missing")
			}

			// "huge_garbage_first_then_json" should actually succeed because
			// extractJSON strips warning lines.  "valid_envelope_missing_end"
			// also succeeds: ParseEnd tolerates missing end (returns 0/0).
			if tc.name == "huge_garbage_first_then_json" {
				if cell.Error != "" {
					t.Errorf("expected success after stripping prefix, got error=%q", cell.Error)
				}
				if !withinPct(cell.Mbps, 905, 1.0) && !withinPct(cell.Mbps, 900, 1.0) {
					t.Errorf("Mbps=%v, want ~900 or ~905", cell.Mbps)
				}
				return
			}
			if tc.name == "valid_envelope_missing_end" {
				// Missing end → ParseEnd returns 0 with no error → cell has
				// Mbps=0 and no Error.  This is the documented degraded path.
				if cell.Mbps != 0 {
					t.Errorf("Mbps=%v, want 0 on missing end", cell.Mbps)
				}
				return
			}

			if cell.Mbps != 0 {
				t.Errorf("Mbps=%v, want 0 on parse failure", cell.Mbps)
			}
			if cell.Error == "" {
				t.Errorf("Error empty, want non-empty diagnostic")
			}
			if tc.wantErrSubstr != "" && !strings.Contains(strings.ToLower(cell.Error), tc.wantErrSubstr) {
				t.Errorf("Error=%q, want substring %q", cell.Error, tc.wantErrSubstr)
			}
			// Cleanup must still run.
			if got := fe.countByKind(stepC); got != 1 {
				t.Errorf("Step C count=%d, want 1", got)
			}
		})
	}
}

// TestExecPair_CancelDuringStepAWait: cancel the parent context while execPair
// is waiting in the post-Step-A file-wait window.  The pair should exit
// promptly with a cancellation error and Step B should never be invoked.
// Step C must still run (cleanup is unconditional).
func TestExecPair_CancelDuringStepAWait(t *testing.T) {
	e, fe := newFakeExecutor(t)
	// Use a very long file wait so we have time to cancel.
	e.SetTimingForTesting(2*time.Second, 10*time.Millisecond, 20*time.Millisecond)

	cancelled := make(chan struct{})
	fe.handler = func(c fakeCall) (string, string, error) {
		// Step A returns immediately; afterwards execPair enters the file-wait.
		// Step B and Step C should still be allowed to invoke the handler.
		return "", "", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
		close(cancelled)
	}()

	task := runOnceCtx(t, ctx, e, snap("10.0.0.1", "10.0.0.2"), "cancel-step-a")
	<-cancelled

	if task.Status != store.StatusCanceled {
		t.Fatalf("status=%s, want canceled (err=%s)", task.Status, task.Error)
	}
	// Step B must NOT have been invoked — we cancelled before the file-wait
	// expired.
	if got := fe.countByKind(stepB); got != 0 {
		t.Errorf("Step B calls=%d, want 0 (cancelled before fetch)", got)
	}
	// Step C must always have run.
	if got := fe.countByKind(stepC); got != 1 {
		t.Errorf("Step C calls=%d, want 1 (cleanup must always run)", got)
	}
}

// TestExecPair_CancelDuringStepBRetry: cancel while we are sleeping between
// Step B retries.  Pair exits, cleanup still runs.
func TestExecPair_CancelDuringStepBRetry(t *testing.T) {
	e, fe := newFakeExecutor(t)
	// Long fetch retry wait so the cancel can land mid-wait.
	e.SetTimingForTesting(50*time.Millisecond, 1*time.Second, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var fetchCount atomic.Int32

	fe.handler = func(c fakeCall) (string, string, error) {
		switch c.Kind {
		case stepA:
			return "", "", nil
		case stepB:
			n := fetchCount.Add(1)
			if n == 1 {
				// Schedule a cancel shortly after the first fetch fails — this
				// will land while execPair is sleeping in fetchRetryWait.
				go func() {
					time.Sleep(100 * time.Millisecond)
					cancel()
				}()
			}
			return "", "", nil // always empty → forces retry
		case stepC:
			return "", "", nil
		}
		return "", "", nil
	}

	task := runOnceCtx(t, ctx, e, snap("10.0.0.1", "10.0.0.2"), "cancel-step-b")
	if task.Status != store.StatusCanceled {
		t.Fatalf("status=%s, want canceled (err=%s)", task.Status, task.Error)
	}
	if got := fe.countByKind(stepB); got >= 4 {
		t.Errorf("Step B calls=%d, want <4 (cancel should have aborted before the full retry budget)", got)
	}
	if got := fe.countByKind(stepC); got != 1 {
		t.Errorf("Step C calls=%d, want 1", got)
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 5. Filename safety / uniqueness — the one-file-per-pair invariant
// ════════════════════════════════════════════════════════════════════════════

// TestExecPair_FilenameUniqueness_AcrossRoundsAndPairs: collect every Step A's
// filename across a multi-round measurement and assert they are all distinct.
// A naming collision would let two concurrent pairs clobber each other's data.
func TestExecPair_FilenameUniqueness_AcrossRoundsAndPairs(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(900e6, 905e6), "", nil
		}
		return "", "", nil
	}

	// N=4 ⇒ 6 pairs across 3 rounds. Concurrent within rounds.
	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"), "filename-task-uuid")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}

	// Extract the redirect target from each Step A invocation.
	stepACalls := fe.callsByKind(stepA)
	if len(stepACalls) != 6 {
		t.Fatalf("Step A calls=%d, want 6", len(stepACalls))
	}
	seen := make(map[string]bool, len(stepACalls))
	for _, c := range stepACalls {
		// cmd looks like ["sh", "-c", "iperf3 ... > /tmp/iperf_<...>.json 2>/dev/null"]
		if len(c.Cmd) != 3 {
			t.Fatalf("Step A cmd shape unexpected: %v", c.Cmd)
		}
		shellCmd := c.Cmd[2]
		// Pull out the redirect target.
		idx := strings.Index(shellCmd, "> /tmp/")
		if idx < 0 {
			t.Errorf("Step A shell missing redirect: %q", shellCmd)
			continue
		}
		rest := shellCmd[idx+2:]        // strip "> "
		end := strings.Index(rest, " ") // up to next space
		if end < 0 {
			end = len(rest)
		}
		fname := strings.TrimSpace(rest[:end])
		if !strings.HasPrefix(fname, "/tmp/iperf_filename-task-uuid_R") {
			t.Errorf("filename %q missing expected prefix", fname)
		}
		if !strings.HasSuffix(fname, ".json") {
			t.Errorf("filename %q missing .json suffix", fname)
		}
		if seen[fname] {
			t.Errorf("filename collision: %q seen twice", fname)
		}
		seen[fname] = true
	}
	if len(seen) != 6 {
		t.Errorf("unique filenames=%d, want 6", len(seen))
	}
}

// TestExecPair_FilenameContainsTaskAndRound: filename must include both the
// task UUID and the round index so concurrent tasks (under a hypothetical
// future relaxation of the global lock) and same-source-different-round runs
// cannot collide.
func TestExecPair_FilenameContainsTaskAndRound(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(900e6, 905e6), "", nil
		}
		return "", "", nil
	}

	const taskID = "abc-def-1234"
	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2", "10.0.0.3"), taskID)
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}

	for _, c := range fe.callsByKind(stepA) {
		shell := c.Cmd[2]
		if !strings.Contains(shell, taskID) {
			t.Errorf("Step A filename missing taskID %q: %q", taskID, shell)
		}
		if !strings.Contains(shell, "_R") {
			t.Errorf("Step A filename missing _R<round>: %q", shell)
		}
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 6. Chaos / additional autonomously-generated edge cases
// ════════════════════════════════════════════════════════════════════════════

// TestStoreAndFetch_StepAErrorButFileEventuallyWritten: Step A's exec stream
// breaks (Tailscale flap) but the iperf3 process inside the pod kept running
// and wrote the file.  Step B succeeds.  This is the load-bearing scenario
// the architecture exists to handle.
func TestStoreAndFetch_StepAErrorButFileEventuallyWritten(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		switch c.Kind {
		case stepA:
			// Simulate Tailscale flap mid-test.
			return "", "", errors.New("error sending request: dial tcp ... operation was canceled")
		case stepB:
			// File was nonetheless written by the pod-side iperf3 process.
			return validBidirJSON(890e6, 880e6), "", nil
		case stepC:
			return "", "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "flap-recover")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)
	cell := res.Matrix["10.0.0.1"]["10.0.0.2"]
	if cell.Error != "" {
		t.Errorf("unexpected error after flap recovery: %s", cell.Error)
	}
	if !withinPct(cell.Mbps, 890, 1.0) {
		t.Errorf("Mbps=%v, want ~890 (the file was successfully fetched despite Step A flap)", cell.Mbps)
	}
}

// TestStoreAndFetch_LargeJSONFile: simulate a real-world iperf3 -J -P 8 output
// with multiple stream entries (~30 KB).  The parser must still extract the
// summary fields correctly.
func TestStoreAndFetch_LargeJSONFile(t *testing.T) {
	// Pad the JSON with many "intervals" entries to push the document into
	// the 30 KB range — exactly the size class real iperf3 emits.
	intervals := make([]map[string]any, 100)
	for i := range intervals {
		intervals[i] = map[string]any{
			"sum": map[string]any{
				"bits_per_second": 900e6 + float64(i),
				"retransmits":     i,
			},
		}
	}
	payload := map[string]any{
		"start":     map[string]any{"test_start": map[string]any{"duration": 10}},
		"intervals": intervals,
		"end": map[string]any{
			"sum_received": map[string]any{
				"bits_per_second": 911e6,
				"sender":          true,
			},
			"sum_received_bidir_reverse": map[string]any{
				"bits_per_second": 922e6,
				"sender":          false,
			},
		},
	}
	b, _ := json.Marshal(payload)

	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return string(b), "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "large-json")
	res := task.Result.(*executor.Result)
	if got := res.Matrix["10.0.0.1"]["10.0.0.2"].Mbps; !withinPct(got, 911, 0.5) {
		t.Errorf("Mbps=%v, want ~911 — large-payload parse failed", got)
	}
}

// TestStoreAndFetch_ZeroBandwidthValidNotAnError: iperf3 successfully ran but
// reported 0 bps (e.g. fully congested link).  This must produce mbps=0 with
// NO error — a successful measurement of a saturated link is not a failure.
func TestStoreAndFetch_ZeroBandwidthValidNotAnError(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(0, 0), "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "zero-bw")
	res := task.Result.(*executor.Result)
	cell := res.Matrix["10.0.0.1"]["10.0.0.2"]
	if cell.Mbps != 0 {
		t.Errorf("Mbps=%v, want 0", cell.Mbps)
	}
	if cell.Error != "" {
		t.Errorf("unexpected error on zero-bandwidth measurement: %s", cell.Error)
	}
}

// TestStoreAndFetch_MultipleRoundsCleanupCount: assert total cleanup calls equals
// total pairs across all rounds — a strong invariant on the deferred Step C.
func TestStoreAndFetch_MultipleRoundsCleanupCount(t *testing.T) {
	const N = 5
	ips := make([]string, N)
	for i := 0; i < N; i++ {
		ips[i] = fmt.Sprintf("172.16.0.%d", i+1)
	}

	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(900e6, 905e6), "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap(ips...), "multi-round")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	pairs := N * (N - 1) / 2
	if got := fe.countByKind(stepC); got != pairs {
		t.Errorf("Step C count=%d, want %d (one cleanup per pair)", got, pairs)
	}
}

// TestRun_ZeroNodes_FailsCleanly: snapshot with no nodes → status=failed,
// no exec calls.  Already covered by an existing test but kept here for the
// QA-grade comprehensive matrix.
func TestRun_ZeroNodes_NoExecCalls(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		t.Fatalf("unexpected exec call: %v", c.Cmd)
		return "", "", nil
	}
	task := runOnce(t, e, snap(), "zero")
	if task.Status != store.StatusFailed {
		t.Fatalf("status=%s, want failed", task.Status)
	}
	if got := len(fe.snapshot()); got != 0 {
		t.Errorf("exec calls=%d, want 0", got)
	}
}

// TestRun_OneNode_FailsCleanly: snapshot with one node → status=failed.
func TestRun_OneNode_NoExecCalls(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		t.Fatalf("unexpected exec call: %v", c.Cmd)
		return "", "", nil
	}
	task := runOnce(t, e, snap("10.0.0.1"), "one")
	if task.Status != store.StatusFailed {
		t.Fatalf("status=%s, want failed", task.Status)
	}
	if got := len(fe.snapshot()); got != 0 {
		t.Errorf("exec calls=%d, want 0", got)
	}
}

// TestExecPair_StepCExecError_DoesNotPropagate: Step C returns an error.  The
// pair's measurement result must still be reported successfully — cleanup
// failure is best-effort and never propagates to the caller.
func TestExecPair_StepCExecError_DoesNotPropagate(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		switch c.Kind {
		case stepA:
			return "", "", nil
		case stepB:
			return validBidirJSON(950e6, 945e6), "", nil
		case stepC:
			return "", "rm: device or resource busy", errors.New("rm failed")
		}
		return "", "", nil
	}
	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2"), "cleanup-err")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s, want completed despite cleanup error", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)
	if !withinPct(res.Matrix["10.0.0.1"]["10.0.0.2"].Mbps, 950, 0.5) {
		t.Errorf("matrix value should still be reported despite cleanup failure")
	}
}

// TestRun_OddNodeCount_DummyPaddingHandled: with N=3 the schedule has 3 rounds,
// one pair per round (one node sits out paired with DUMMY).  Verify that no
// Step is ever invoked against the DUMMY node and the matrix is fully populated.
func TestRun_OddNodeCount_DummyPaddingHandled(t *testing.T) {
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		// Detect any DUMMY token in the shell command (Step A) or filename (Step B).
		for _, arg := range c.Cmd {
			if strings.Contains(arg, "DUMMY") {
				t.Errorf("DUMMY leaked into exec call: %v", c.Cmd)
			}
		}
		if c.Kind == stepB {
			return validBidirJSON(900e6, 905e6), "", nil
		}
		return "", "", nil
	}

	task := runOnce(t, e, snap("10.0.0.1", "10.0.0.2", "10.0.0.3"), "odd")
	if task.Status != store.StatusCompleted {
		t.Fatalf("status=%s err=%s", task.Status, task.Error)
	}
	res := task.Result.(*executor.Result)
	// N=3 → 3 rows × 2 peers each = 6 cells; no diagonal entries.
	totalCells := 0
	for src, row := range res.Matrix {
		if _, exists := row[src]; exists {
			t.Errorf("diagonal matrix[%s][%s] must be absent", src, src)
		}
		totalCells += len(row)
	}
	if totalCells != 6 {
		t.Errorf("total cells=%d, want 6 for N=3 (N*(N-1), no diagonal)", totalCells)
	}
}

// TestRun_DuplicateIPsInSnapshot_NoCrash: defence in depth — DiscoverNodes
// dedupes, but if the snapshot somehow reaches Run() with duplicate IPs the
// executor must still produce a sensible result without crashing.
//
// The current scheduler emits pairs based on the supplied list; duplicates
// would create self-pairs (A→A) which makes no sense to measure.  The test
// codifies the existing behaviour: the run completes without panic.
func TestRun_DuplicateIPsInSnapshot_NoCrash(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on duplicate IPs: %v", r)
		}
	}()
	s := &executor.NodeSnapshot{
		IPs:      []string{"10.0.0.1", "10.0.0.1", "10.0.0.2"},
		PodNames: map[string]string{"10.0.0.1": "iperf-1", "10.0.0.2": "iperf-2"},
	}
	e, fe := newFakeExecutor(t)
	fe.handler = func(c fakeCall) (string, string, error) {
		if c.Kind == stepB {
			return validBidirJSON(900e6, 905e6), "", nil
		}
		return "", "", nil
	}
	task := runOnce(t, e, s, "dup")
	// We don't strongly assert status here; the contract is "do not crash".
	t.Logf("dup-IPs run finished: status=%s err=%s", task.Status, task.Error)
}
