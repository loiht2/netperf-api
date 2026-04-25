// Package iperf3 parses the JSON output produced by `iperf3 -J [--bidir]`.
//
// Field-name reference (verified against iperf3 3.12 with --bidir):
//
//	end.sum_sent                  — forward direction sender stats  (client → server)
//	end.sum_received              — forward direction receiver stats (client → server)
//	end.sum_sent_bidir_reverse    — reverse direction sender stats  (server → client)
//	end.sum_received_bidir_reverse— reverse direction receiver stats(server → client)
//
// Note: individual intervals use a different key ("sum_bidir_reverse") but the
// final end-level aggregates use the "_sent_" / "_received_" variants above.
package iperf3

import "encoding/json"

// Output is the top-level JSON document written to stdout by iperf3.
type Output struct {
	// Error is non-empty when iperf3 itself reports a connection failure.
	Error string `json:"error"`
	End   End    `json:"end"`
}

// End aggregates the final statistics across all streams.
type End struct {
	// Forward direction (client → server).
	SumSent     Sum `json:"sum_sent"`
	SumReceived Sum `json:"sum_received"`

	// Reverse direction (server → client) — only present with --bidir.
	// iperf3 3.12 uses "sum_sent_bidir_reverse" (sender-side stats from the
	// server) and "sum_received_bidir_reverse" (receiver-side stats on the
	// client). We use the sender-side value for consistency with sum_sent.
	SumSentBidirReverse Sum `json:"sum_sent_bidir_reverse"`
	SumRecvBidirReverse Sum `json:"sum_received_bidir_reverse"`
}

// Sum holds the aggregate throughput statistics for one direction.
type Sum struct {
	Bytes         int64   `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	Seconds       float64 `json:"seconds"`
	Retransmits   int     `json:"retransmits,omitempty"`
	Sender        bool    `json:"sender"`
}

// Parse unmarshals raw iperf3 JSON bytes into an Output struct.
func Parse(data []byte) (*Output, error) {
	var out Output
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BitsToMbps converts bits-per-second to Mbit/s (1 Mbps = 10^6 bps).
func BitsToMbps(bps float64) float64 { return bps / 1e6 }
