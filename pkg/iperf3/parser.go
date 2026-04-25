// Package iperf3 parses the JSON output produced by `iperf3 -J [--bidir]`.
//
// iperf3 occasionally emits plain-text warning lines before the opening '{'
// (e.g. "Warning: --bidir is experimental and may change in future versions").
// Parse strips that prefix before unmarshalling so those warnings never cause
// a silent parse failure.
//
// The End section is retained as json.RawMessage and decoded lazily by ParseEnd
// via a map[string]json.RawMessage. This makes the parser tolerant of missing or
// differently-typed fields across iperf3 versions and prevents a missing
// "sum_sent_bidir_reverse" from silently zeroing the entire measurement.
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

// BidirResult holds all four directed throughput values from one --bidir exec.
//
// Field mapping (measured at the client / source pod):
//
//	FwdSentBps  ← end.sum_sent                   (source→target sender)
//	FwdRecvBps  ← end.sum_received                (source←target receiver)
//	RevSentBps  ← end.sum_sent_bidir_reverse       (target→source sender)
//	RevRecvBps  ← end.sum_received_bidir_reverse   (target←source receiver)
type BidirResult struct {
	FwdSentBps float64
	FwdRecvBps float64
	RevSentBps float64
	RevRecvBps float64
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
		FwdSentBps: bpsFromSumField(endMap, "sum_sent"),
		FwdRecvBps: bpsFromSumField(endMap, "sum_received"),
		RevSentBps: bpsFromSumField(endMap, "sum_sent_bidir_reverse"),
		RevRecvBps: bpsFromSumField(endMap, "sum_received_bidir_reverse"),
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
