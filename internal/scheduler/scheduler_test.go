package scheduler_test

import (
	"fmt"
	"testing"

	"github.com/netperf/netperf-api/internal/scheduler"
)

// pairKey returns a canonical, order-independent key for a Pair.
// "A|B" and "B|A" collapse to the same key so we can detect duplicates.
func pairKey(p scheduler.Pair) string {
	if p.Source < p.Target {
		return p.Source + "|" + p.Target
	}
	return p.Target + "|" + p.Source
}

// ── Table-Driven Tests ────────────────────────────────────────────────────────

// TestGenerateSchedule_RoundRobin is the primary algorithmic correctness test.
// It covers both EVEN and ODD node counts from N=2 to N=8 and asserts five
// invariants for every generated schedule.
func TestGenerateSchedule_RoundRobin(t *testing.T) {
	tests := []struct {
		// nodeCount is the number of real nodes fed to GenerateSchedule.
		nodeCount int
		// wantRounds: even N → N-1 rounds; odd N → padded to N+1, so N rounds.
		wantRounds int
		// wantPairs: always N*(N-1)/2 unique pairs.
		wantPairs int
	}{
		{nodeCount: 2, wantRounds: 1, wantPairs: 1},  // even
		{nodeCount: 3, wantRounds: 3, wantPairs: 3},  // odd  → pads to 4
		{nodeCount: 4, wantRounds: 3, wantPairs: 6},  // even
		{nodeCount: 5, wantRounds: 5, wantPairs: 10}, // odd  → pads to 6
		{nodeCount: 6, wantRounds: 5, wantPairs: 15}, // even
		{nodeCount: 7, wantRounds: 7, wantPairs: 21}, // odd  → pads to 8
		{nodeCount: 8, wantRounds: 7, wantPairs: 28}, // even
	}

	for _, tc := range tests {
		tc := tc
		t.Run(fmt.Sprintf("N=%d", tc.nodeCount), func(t *testing.T) {
			t.Parallel()

			nodes := make([]string, tc.nodeCount)
			for i := range nodes {
				nodes[i] = fmt.Sprintf("node%02d", i)
			}

			sched := scheduler.GenerateSchedule(nodes)

			// ── Invariant 1: correct number of rounds ────────────────────────
			if got := len(sched); got != tc.wantRounds {
				t.Errorf("rounds: want %d, got %d", tc.wantRounds, got)
			}

			seen := make(map[string]bool)

			for rIdx, round := range sched {
				roundNodes := make(map[string]bool)

				for _, p := range round {
					// ── Invariant 2: no node appears twice in the same round ─
					if roundNodes[p.Source] || roundNodes[p.Target] {
						t.Errorf("round %d: node collision — source=%s target=%s",
							rIdx+1, p.Source, p.Target)
					}
					roundNodes[p.Source] = true
					roundNodes[p.Target] = true

					// ── Invariant 3: DUMMY must never appear in a real pair ──
					if p.Source == scheduler.Dummy || p.Target == scheduler.Dummy {
						t.Errorf("round %d: DUMMY node leaked into pair %+v", rIdx+1, p)
					}

					// ── Invariant 4: all pairs are globally unique ───────────
					key := pairKey(p)
					if seen[key] {
						t.Errorf("duplicate pair %q first appeared before round %d", key, rIdx+1)
					}
					seen[key] = true
				}
			}

			// ── Invariant 5: exactly N*(N-1)/2 unique pairs total ────────────
			if got := len(seen); got != tc.wantPairs {
				t.Errorf("unique pairs: want %d, got %d", tc.wantPairs, got)
			}
		})
	}
}

// TestGenerateSchedule_ExactOrder verifies the exact pair ordering for N=4
// against the worked example from the project specification:
//
//	Round 1: [A B C D] → (A,D), (B,C)
//	Round 2: [A D B C] → (A,C), (D,B)
//	Round 3: [A C D B] → (A,B), (C,D)
func TestGenerateSchedule_ExactOrder(t *testing.T) {
	sched := scheduler.GenerateSchedule([]string{"A", "B", "C", "D"})

	if len(sched) != 3 {
		t.Fatalf("want 3 rounds, got %d", len(sched))
	}

	want := [][]scheduler.Pair{
		{{Source: "A", Target: "D"}, {Source: "B", Target: "C"}},
		{{Source: "A", Target: "C"}, {Source: "D", Target: "B"}},
		{{Source: "A", Target: "B"}, {Source: "C", Target: "D"}},
	}

	for r, round := range sched {
		if len(round) != len(want[r]) {
			t.Errorf("round %d: want %d pairs, got %d", r+1, len(want[r]), len(round))
			continue
		}
		for i, got := range round {
			w := want[r][i]
			if got != w {
				t.Errorf("round %d pair %d: want {%s→%s}, got {%s→%s}",
					r+1, i+1, w.Source, w.Target, got.Source, got.Target)
			}
		}
	}
}

// TestGenerateSchedule_OddExact verifies N=3 (odd) uses the DUMMY correctly:
// the schedule must contain exactly 3 unique pairs with no idle round wasted.
func TestGenerateSchedule_OddExact(t *testing.T) {
	// With N=3 → padded to [A, B, C, DUMMY]:
	// Round 1: (A,DUMMY)→skip, (B,C)→keep  → [(B,C)]
	// Round 2: (A,C), (DUMMY,B)→skip        → [(A,C)]
	// Round 3: (A,B), (C,DUMMY)→skip        → [(A,B)]
	sched := scheduler.GenerateSchedule([]string{"A", "B", "C"})

	if len(sched) != 3 {
		t.Fatalf("want 3 rounds for N=3, got %d", len(sched))
	}
	for r, round := range sched {
		if len(round) != 1 {
			t.Errorf("round %d: odd N=3 should have exactly 1 pair, got %d", r+1, len(round))
		}
	}

	allPairs := make(map[string]bool)
	for _, round := range sched {
		for _, p := range round {
			allPairs[pairKey(p)] = true
		}
	}
	if len(allPairs) != 3 {
		t.Errorf("N=3 must produce 3 unique pairs, got %d: %v", len(allPairs), allPairs)
	}
}

// TestGenerateSchedule_EdgeCases covers degenerate inputs.
func TestGenerateSchedule_EdgeCases(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if s := scheduler.GenerateSchedule(nil); len(s) != 0 {
			t.Errorf("nil → want empty schedule, got %d rounds", len(s))
		}
	})
	t.Run("single node", func(t *testing.T) {
		if s := scheduler.GenerateSchedule([]string{"solo"}); len(s) != 0 {
			t.Errorf("1 node → want empty schedule, got %d rounds", len(s))
		}
	})
	t.Run("input slice not mutated", func(t *testing.T) {
		// GenerateSchedule must not modify the caller's slice.
		orig := []string{"X", "Y", "Z"}
		before := make([]string, len(orig))
		copy(before, orig)
		scheduler.GenerateSchedule(orig)
		for i, v := range orig {
			if v != before[i] {
				t.Errorf("input slice mutated at index %d: was %q, now %q", i, before[i], v)
			}
		}
	})
}
