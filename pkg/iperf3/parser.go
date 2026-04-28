// Package iperf3 parses the JSON output produced by `iperf3 -J [--bidir]`.
//
// iperf3 occasionally emits plain-text warning lines before the opening '{'
// (e.g. "Warning: --bidir is experimental and may change in future versions").
// Parse strips that prefix before unmarshalling so those warnings never cause
// a silent parse failure.
//
// The End section is retained as json.RawMessage and decoded lazily by ParseEnd
// via a map[string]json.RawMessage. This makes the parser tolerant of missing or
// differently-typed fields across iperf3 versions, so for example a missing
// "sum_received_bidir_reverse" silently yields 0 for the reverse-direction
// receiver instead of failing the whole document.
//
// Only the two receiver-side counters (sum_received and sum_received_bidir_reverse)
// are parsed; the sender-side counters are not used by this project.
package iperf3

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Output is the top-level JSON document written to stdout by iperf3 -J.
type Output struct {
	// Error is non-empty when iperf3 itself reports a connection failure.
	Error string `json:"error"`
	// End is kept raw so ParseEnd can extract fields tolerantly.
	End json.RawMessage `json:"end"`
}

// BidirResult holds the two receiver-side throughput values from one --bidir
// iperf3 exec — exactly the data needed to populate a directional bandwidth
// matrix where matrix[Source][Target] = bandwidth Target received from Source.
//
// Sender-side counters (sum_sent / sum_sent_bidir_reverse) are intentionally
// not parsed: on a healthy link they are bounded by the receiver and add no
// information for our use case.
//
// iperf3 -J --bidir field mapping (forward = the stream that the client
// initiated with -c, reverse = the second stream added by --bidir):
//
//	ToTargetBps ← end.sum_received               receiver-side counter for the
//	                                             FORWARD stream (source → target)
//	                                             = bandwidth target received from source
//	ToSourceBps ← end.sum_received_bidir_reverse receiver-side counter for the
//	                                             REVERSE stream (target → source)
//	                                             = bandwidth source received from target
type BidirResult struct {
	ToTargetBps float64
	ToSourceBps float64
}

// extractJSON returns the slice of data from the first '{' to the last '}',
// discarding any leading warning lines iperf3 prints before its JSON document.
func extractJSON(data []byte) ([]byte, error) {
	start := bytes.IndexByte(data, '{')
	if start < 0 {
		return nil, fmt.Errorf("no JSON object found in iperf3 output")
	}
	end := bytes.LastIndexByte(data, '}')
	if end <= start {
		return nil, fmt.Errorf("malformed iperf3 output: closing '}' not found after '{'")
	}
	return data[start : end+1], nil
}

// Parse strips any leading non-JSON text and unmarshals the iperf3 -J document.
func Parse(data []byte) (*Output, error) {
	jsonData, err := extractJSON(data)
	if err != nil {
		return nil, err
	}
	var out Output
	if err := json.Unmarshal(jsonData, &out); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &out, nil
}

// ParseEnd extracts bidirectional throughput from the raw "end" JSON section.
//
// Using map[string]json.RawMessage means:
//   - Missing fields silently contribute 0 bps instead of failing the whole parse.
//   - Unexpected extra fields are ignored.
//   - Both object form {"bits_per_second": N} and array form [{...}] are handled.
func ParseEnd(endRaw json.RawMessage) (BidirResult, error) {
	if len(endRaw) == 0 {
		return BidirResult{}, fmt.Errorf("end section absent from iperf3 output")
	}
	var endMap map[string]json.RawMessage
	if err := json.Unmarshal(endRaw, &endMap); err != nil {
		return BidirResult{}, fmt.Errorf("parsing end section: %w", err)
	}
	return BidirResult{
		ToTargetBps: bpsFromSumField(endMap, "sum_received"),
		ToSourceBps: bpsFromSumField(endMap, "sum_received_bidir_reverse"),
	}, nil
}

// bpsFromSumField safely extracts bits_per_second from a named sum object inside
// the end map. It handles two layouts seen across iperf3 builds:
//
//	object form : {"bits_per_second": 1.23e9, "sender": true, ...}
//	array form  : [{"bits_per_second": 1.23e9, ...}]  (first element used)
//
// IMPORTANT: iperf3 sum objects contain mixed-type fields (e.g. "sender": true,
// "retransmits": 11). Using map[string]json.Number would fail because it rejects
// boolean values. We decode into map[string]json.RawMessage so each field is
// captured as raw bytes, then extract only "bits_per_second" as a json.Number.
//
// Returns 0 when the field is absent or bits_per_second cannot be parsed.
func bpsFromSumField(endMap map[string]json.RawMessage, key string) float64 {
	raw, ok := endMap[key]
	if !ok || len(raw) == 0 {
		return 0
	}
	// Primary path: object form — decode all fields as raw bytes, then read
	// bits_per_second specifically to avoid type errors on boolean/string fields.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		if bpsRaw, ok := obj["bits_per_second"]; ok {
			var n json.Number
			if err := json.Unmarshal(bpsRaw, &n); err == nil {
				if f, err := n.Float64(); err == nil {
					return f
				}
			}
		}
		return 0
	}
	// Fallback: array form — use the first element.
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		if bpsRaw, ok := arr[0]["bits_per_second"]; ok {
			var n json.Number
			if err := json.Unmarshal(bpsRaw, &n); err == nil {
				if f, err := n.Float64(); err == nil {
					return f
				}
			}
		}
	}
	return 0
}

// BitsToMbps converts bits-per-second to Mbit/s (1 Mbps = 10^6 bps).
func BitsToMbps(bps float64) float64 { return bps / 1e6 }
