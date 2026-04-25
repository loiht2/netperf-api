// Package scheduler implements the circle (round-robin tournament) algorithm
// for generating a conflict-free parallel test schedule.
package scheduler

const Dummy = "DUMMY"

// Pair holds one bidirectional iperf3 test: iperf3 runs on Source, connects to Target.
type Pair struct {
	Source string
	Target string
}

// Round is a set of Pairs that are safe to execute concurrently (no node appears twice).
type Round []Pair

// Schedule is the ordered list of Rounds for the full test plan.
type Schedule []Round

// GenerateSchedule returns N-1 rounds covering all N*(N-1)/2 unique node pairs.
//
// Algorithm (circle / round-robin tournament):
//   - If N is odd, append a synthetic "DUMMY" node to make N even.
//   - Each round pairs circle[i] with circle[N-1-i] for i in [0, N/2).
//   - Pairs that involve DUMMY are silently discarded (that real node idles).
//   - Between rounds, keep circle[0] fixed and rotate the remaining N-1 positions
//     clockwise: the tail element jumps to index 1, shifting 1..N-2 right by one.
func GenerateSchedule(nodes []string) Schedule {
	if len(nodes) < 2 {
		return Schedule{}
	}

	// Work on a copy so the caller's slice is untouched.
	circle := make([]string, len(nodes))
	copy(circle, nodes)

	if len(circle)%2 != 0 {
		circle = append(circle, Dummy)
	}
	n := len(circle)        // guaranteed even
	rounds := n - 1         // total rounds needed

	schedule := make(Schedule, 0, rounds)

	for r := 0; r < rounds; r++ {
		var round Round
		for i := 0; i < n/2; i++ {
			a, b := circle[i], circle[n-1-i]
			if a == Dummy || b == Dummy {
				continue // this real node sits idle this round
			}
			round = append(round, Pair{Source: a, Target: b})
		}
		schedule = append(schedule, round)

		// Clockwise rotation: keep index 0 fixed.
		//   circle[n-1] → circle[1]
		//   circle[1..n-2] → circle[2..n-1]
		//
		// Go's copy uses memmove semantics, so overlapping slices are handled
		// correctly even though src and dst share the same backing array.
		last := circle[n-1]
		copy(circle[2:], circle[1:n-1]) // shift indices 1..n-2 right by one
		circle[1] = last
	}

	return schedule
}
