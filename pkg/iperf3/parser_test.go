package iperf3_test

import (
	"encoding/json"
	"testing"

	"github.com/netperf/netperf-api/pkg/iperf3"
)

// successJSON mirrors the actual end-section structure produced by iperf3 3.12
// --bidir -J as captured from the live cluster pods. Only the fields our parser
// reads are included; the rest (cpu_utilization_percent, etc.) are omitted to
// keep the fixture focused.
const successJSON = `{
	"start": {},
	"intervals": [],
	"end": {
		"streams": [],
		"sum_sent": {
			"start": 0,
			"end": 10.000279,
			"seconds": 10.000279,
			"bytes": 1142933504,
			"bits_per_second": 914346934.0980172,
			"retransmits": 11,
			"sender": true
		},
		"sum_received": {
			"start": 0,
			"end": 10.000279,
			"seconds": 10.000279,
			"bytes": 1137373184,
			"bits_per_second": 909898725.866858,
			"sender": false
		},
		"sum_sent_bidir_reverse": {
			"start": 0,
			"end": 10.000279,
			"seconds": 10.000279,
			"bytes": 1163642880,
			"bits_per_second": 930914229.4635547,
			"retransmits": 4,
			"sender": true
		},
		"sum_received_bidir_reverse": {
			"start": 0,
			"end": 10.000279,
			"seconds": 10.000279,
			"bytes": 1157688320,
			"bits_per_second": 926151052.768152,
			"sender": false
		},
		"cpu_utilization_percent": {},
		"sender_tcp_congestion": "cubic",
		"receiver_tcp_congestion": "cubic"
	}
}`

// warningPrefixJSON simulates iperf3 versions that print a warning line before
// the opening '{' (e.g. experimental --bidir warning on older builds).
const warningPrefixJSON = `Warning: --bidir option is in use and is an experimental feature with known problems. Use with caution.
` + successJSON

// connectionRefusedJSON is what iperf3 -J emits when it cannot reach the server.
const connectionRefusedJSON = `{
	"error": "unable to connect to server: Connection refused"
}`

// connectionRefusedWithWarning combines warning prefix + connection error,
// the worst-case combination that previously caused silent parse failure.
const connectionRefusedWithWarning = `Warning: --bidir option is in use and is an experimental feature with known problems. Use with caution.
{
	"error": "unable to connect to server: Connection refused"
}`

// ── Parse ─────────────────────────────────────────────────────────────────────

func TestParse_CleanSuccess(t *testing.T) {
	out, err := iperf3.Parse([]byte(successJSON))
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Errorf("out.Error should be empty, got %q", out.Error)
	}
	if len(out.End) == 0 {
		t.Error("out.End should be non-empty")
	}
}

func TestParse_WarningPrefix(t *testing.T) {
	out, err := iperf3.Parse([]byte(warningPrefixJSON))
	if err != nil {
		t.Fatalf("Parse with warning prefix returned error: %v", err)
	}
	if out.Error != "" {
		t.Errorf("out.Error should be empty, got %q", out.Error)
	}
}

func TestParse_ConnectionRefused(t *testing.T) {
	out, err := iperf3.Parse([]byte(connectionRefusedJSON))
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	if out.Error == "" {
		t.Error("out.Error should be non-empty for connection refused")
	}
}

func TestParse_ConnectionRefusedWithWarning(t *testing.T) {
	out, err := iperf3.Parse([]byte(connectionRefusedWithWarning))
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	if out.Error == "" {
		t.Error("out.Error should be non-empty when iperf3 reports connection failure")
	}
}

func TestParse_EmptyInput(t *testing.T) {
	_, err := iperf3.Parse([]byte(""))
	if err == nil {
		t.Error("Parse should return error for empty input")
	}
}

func TestParse_NoJSON(t *testing.T) {
	_, err := iperf3.Parse([]byte("Warning: something went wrong\nno JSON follows"))
	if err == nil {
		t.Error("Parse should return error when no JSON object is present")
	}
}

func TestParse_MalformedJSON(t *testing.T) {
	_, err := iperf3.Parse([]byte(`{"end": {"sum_sent": `))
	if err == nil {
		t.Error("Parse should return error for truncated JSON")
	}
}

// ── ParseEnd ─────────────────────────────────────────────────────────────────

func TestParseEnd_BothDirections(t *testing.T) {
	out, err := iperf3.Parse([]byte(successJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	bidir, err := iperf3.ParseEnd(out.End)
	if err != nil {
		t.Fatalf("ParseEnd returned error: %v", err)
	}

	// Values come from the receiver-side fields of successJSON above:
	//   ToTargetBps ← end.sum_received                ≈ 909898725.87
	//   ToSourceBps ← end.sum_received_bidir_reverse  ≈ 926151052.77
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"ToTargetBps", bidir.ToTargetBps, 909898725.866858},
		{"ToSourceBps", bidir.ToSourceBps, 926151052.768152},
	}
	for _, c := range cases {
		if c.got <= 0 {
			t.Errorf("%s: expected > 0, got %f", c.name, c.got)
		}
		// Allow 0.1% tolerance for floating-point round-trip through JSON.
		if diff := c.got - c.want; diff > c.want*0.001 || diff < -c.want*0.001 {
			t.Errorf("%s: got %f, want ~%f", c.name, c.got, c.want)
		}
	}
}

func TestParseEnd_MissingReverseField(t *testing.T) {
	// Simulates a unidirectional test (no --bidir) or an older iperf3 build
	// where sum_received_bidir_reverse is absent. ToSourceBps should be 0, not
	// an error — the consumer can decide whether 0 indicates "no reverse data"
	// or a genuine 0 Mbps link.
	endRaw := json.RawMessage(`{
		"sum_received": {"bits_per_second": 950000000.0}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd should not error on missing reverse field: %v", err)
	}
	if bidir.ToTargetBps != 950000000.0 {
		t.Errorf("ToTargetBps: got %f, want 950000000.0", bidir.ToTargetBps)
	}
	if bidir.ToSourceBps != 0 {
		t.Errorf("ToSourceBps: got %f, want 0 (field absent)", bidir.ToSourceBps)
	}
}

func TestParseEnd_NilEnd(t *testing.T) {
	// When iperf3 reports an error the "end" section is absent → nil RawMessage.
	_, err := iperf3.ParseEnd(nil)
	if err == nil {
		t.Error("ParseEnd should return error for nil/empty end section")
	}
}

func TestParseEnd_UnexpectedExtraFields(t *testing.T) {
	// Extra unknown fields (including the now-unused sender-side counters) must
	// not cause a failure.
	endRaw := json.RawMessage(`{
		"sum_sent":                   {"bits_per_second": 500000000.0, "unknown_future_field": true},
		"sum_received":               {"bits_per_second": 480000000.0},
		"sum_sent_bidir_reverse":     {"bits_per_second": 510000000.0},
		"sum_received_bidir_reverse": {"bits_per_second": 495000000.0},
		"new_section_we_dont_know_about": {"foo": "bar"}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd should tolerate unknown fields: %v", err)
	}
	if bidir.ToTargetBps != 480000000.0 {
		t.Errorf("ToTargetBps: got %f, want 480000000.0", bidir.ToTargetBps)
	}
	if bidir.ToSourceBps != 495000000.0 {
		t.Errorf("ToSourceBps: got %f, want 495000000.0", bidir.ToSourceBps)
	}
}

// ── BitsToMbps ───────────────────────────────────────────────────────────────

func TestBitsToMbps(t *testing.T) {
	cases := []struct {
		bps  float64
		mbps float64
	}{
		{1_000_000_000, 1000},
		{914_346_934, 914.346934},
		{0, 0},
	}
	for _, c := range cases {
		got := iperf3.BitsToMbps(c.bps)
		if got != c.mbps {
			t.Errorf("BitsToMbps(%f) = %f, want %f", c.bps, got, c.mbps)
		}
	}
}

// ── Adversarial / corrupted inputs ────────────────────────────────────────
//
// These tests defend against JSON shapes that have been observed in the wild
// (or theorised by chaos-engineering principles) and that should NEVER cause
// a panic, a silent zero, or a crash of the executor goroutine. Each test
// pins one specific failure mode and asserts the parser returns the expected
// error or the expected sentinel value.

func TestParse_OnlyOpeningBrace(t *testing.T) {
	_, err := iperf3.Parse([]byte("{"))
	if err == nil {
		t.Error("Parse should error for a single '{' with no closing '}'")
	}
}

func TestParse_OnlyClosingBrace(t *testing.T) {
	_, err := iperf3.Parse([]byte("}"))
	if err == nil {
		t.Error("Parse should error when only a closing '}' is present")
	}
}

func TestParse_WhitespaceOnly(t *testing.T) {
	_, err := iperf3.Parse([]byte("   \n\t  "))
	if err == nil {
		t.Error("Parse should error on pure whitespace")
	}
}

func TestParse_NotJSONAtAll(t *testing.T) {
	_, err := iperf3.Parse([]byte("iperf3: error - some textual error message"))
	if err == nil {
		t.Error("Parse should error when input is plain text with no JSON")
	}
}

func TestParse_ManyNestedJunkLines(t *testing.T) {
	// Worst-case real-world output: many stderr-style warnings before JSON.
	junk := "Warning: A\nWarning: B\nWarning: C\nWarning: D\n" + successJSON
	out, err := iperf3.Parse([]byte(junk))
	if err != nil {
		t.Fatalf("Parse with multiple warning lines should succeed: %v", err)
	}
	if out.Error != "" {
		t.Errorf("out.Error should be empty, got %q", out.Error)
	}
}

func TestParse_TrailingGarbageAfterJSON(t *testing.T) {
	// Some shells append a stderr line AFTER the closing brace.
	mixed := successJSON + "\nWarning: extra trailing line\n"
	out, err := iperf3.Parse([]byte(mixed))
	if err != nil {
		t.Fatalf("Parse with trailing garbage should succeed (lastIndex of '}' wins): %v", err)
	}
	if out.Error != "" {
		t.Errorf("out.Error should be empty, got %q", out.Error)
	}
}

func TestParse_NestedObjectClosingBraces(t *testing.T) {
	// extractJSON uses LastIndex of '}', which must still pick the outermost
	// closing brace even when many appear inside nested objects.
	out, err := iperf3.Parse([]byte(successJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out.Error != "" {
		t.Errorf("unexpected error: %s", out.Error)
	}
}

func TestParseEnd_EmptyObject(t *testing.T) {
	// Valid JSON, but no fields at all → all zeros, no error.
	bidir, err := iperf3.ParseEnd(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ParseEnd({}): %v", err)
	}
	if bidir.ToTargetBps != 0 || bidir.ToSourceBps != 0 {
		t.Errorf("empty end should yield all zeros, got %+v", bidir)
	}
}

func TestParseEnd_NotAnObject(t *testing.T) {
	// "end" being a string, number, array, or boolean is nonsensical and must
	// produce an error (not a panic, not silent zeros). NOTE: JSON null is
	// excluded — Go unmarshals null into a map without error, leaving an empty
	// map (covered by TestParseEnd_NullValue below).
	cases := []json.RawMessage{
		json.RawMessage(`"a string"`),
		json.RawMessage(`12345`),
		json.RawMessage(`[1, 2, 3]`),
		json.RawMessage(`true`),
	}
	for _, raw := range cases {
		_, err := iperf3.ParseEnd(raw)
		if err == nil {
			t.Errorf("ParseEnd(%s) should return an error, got nil", string(raw))
		}
	}
}

func TestParseEnd_NullValue(t *testing.T) {
	// JSON null unmarshals into a nil map — no fields to read, all bps zero,
	// and crucially no panic / no error (the upstream caller still gets a
	// well-formed BidirResult{}).
	bidir, err := iperf3.ParseEnd(json.RawMessage(`null`))
	if err != nil {
		t.Fatalf("ParseEnd(null) should succeed with zero values: %v", err)
	}
	if bidir.ToTargetBps != 0 || bidir.ToSourceBps != 0 {
		t.Errorf("null end → all zeros expected, got %+v", bidir)
	}
}

func TestParseEnd_BpsAsNumericString(t *testing.T) {
	// Go's json.Number is lenient: a quoted decimal string parses as that
	// number. This is documented behaviour and we want to LOCK IT IN — a
	// future Go upgrade that tightens this would silently regress real iperf3
	// builds that stringify large counters.
	endRaw := json.RawMessage(`{
		"sum_received":               {"bits_per_second": "910000000"},
		"sum_received_bidir_reverse": {"bits_per_second": 920000000.0}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd: %v", err)
	}
	if bidir.ToTargetBps != 910000000.0 {
		t.Errorf(`numeric string "910000000" should parse to 910M, got %f`, bidir.ToTargetBps)
	}
	if bidir.ToSourceBps != 920000000.0 {
		t.Errorf("ToSourceBps: want 920M, got %f", bidir.ToSourceBps)
	}
}

func TestParseEnd_BpsAsNonNumericString(t *testing.T) {
	// A string that does NOT contain a valid number must be ignored (return 0)
	// rather than crashing or producing NaN.
	endRaw := json.RawMessage(`{"sum_received": {"bits_per_second": "not-a-number"}}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd should not error on garbage string bps: %v", err)
	}
	if bidir.ToTargetBps != 0 {
		t.Errorf("garbage string bps should yield 0, got %f", bidir.ToTargetBps)
	}
}

func TestParseEnd_BpsAsNull(t *testing.T) {
	endRaw := json.RawMessage(`{
		"sum_received":               {"bits_per_second": null},
		"sum_received_bidir_reverse": {"bits_per_second": 950000000.0}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd: %v", err)
	}
	if bidir.ToTargetBps != 0 {
		t.Errorf("null bits_per_second should yield 0, got %f", bidir.ToTargetBps)
	}
	if bidir.ToSourceBps != 950000000.0 {
		t.Errorf("ToSourceBps: want 950000000.0, got %f", bidir.ToSourceBps)
	}
}

func TestParseEnd_BpsAsBoolean(t *testing.T) {
	// This is the EXACT shape that bit us in production: "sender": true used
	// to break the entire decode when we fed sum objects through json.Number.
	// With map[string]json.RawMessage the boolean is captured but ignored.
	endRaw := json.RawMessage(`{
		"sum_received": {"bits_per_second": true, "sender": false}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd should not error on bool bits_per_second: %v", err)
	}
	if bidir.ToTargetBps != 0 {
		t.Errorf("bool bits_per_second should yield 0, got %f", bidir.ToTargetBps)
	}
}

func TestParseEnd_BpsAsArrayLayout(t *testing.T) {
	// Some iperf3 builds wrap sum entries in an array; bpsFromSumField has a
	// fallback path for this. Verify the first element is used.
	endRaw := json.RawMessage(`{
		"sum_received":               [{"bits_per_second": 800000000.0}, {"bits_per_second": 1.0}],
		"sum_received_bidir_reverse": {"bits_per_second": 850000000.0}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd: %v", err)
	}
	if bidir.ToTargetBps != 800000000.0 {
		t.Errorf("array layout: want first element's bps 800M, got %f", bidir.ToTargetBps)
	}
	if bidir.ToSourceBps != 850000000.0 {
		t.Errorf("ToSourceBps: want 850M, got %f", bidir.ToSourceBps)
	}
}

func TestParseEnd_VeryLargeBps(t *testing.T) {
	// 100 Gbit/s ≈ 1e11 — well within float64 precision; must round-trip.
	endRaw := json.RawMessage(`{
		"sum_received": {"bits_per_second": 100000000000.0}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd: %v", err)
	}
	if bidir.ToTargetBps != 100000000000.0 {
		t.Errorf("100 Gbit/s round-trip: got %f", bidir.ToTargetBps)
	}
}

func TestParseEnd_NegativeBps(t *testing.T) {
	// Nonsensical but valid JSON — parser passes the value through unchanged.
	// The executor / consumer is responsible for treating negatives as errors.
	endRaw := json.RawMessage(`{"sum_received": {"bits_per_second": -1}}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd: %v", err)
	}
	if bidir.ToTargetBps >= 0 {
		t.Errorf("negative value should pass through, got %f", bidir.ToTargetBps)
	}
}

func TestParse_ConnectionRefusedHasNoEnd(t *testing.T) {
	// On connection failure iperf3 emits {"error": "..."} with no "end".
	// Parse must succeed, ParseEnd on the (empty) End must error.
	out, err := iperf3.Parse([]byte(connectionRefusedJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out.Error == "" {
		t.Fatal("expected non-empty Error on connection refused")
	}
	if _, err := iperf3.ParseEnd(out.End); err == nil {
		t.Error("ParseEnd on absent end should error")
	}
}

// ── Round-trip: Parse → ParseEnd → BitsToMbps ─────────────────────────────

func TestRoundTrip_WarningPrefix(t *testing.T) {
	out, err := iperf3.Parse([]byte(warningPrefixJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected iperf3 error: %s", out.Error)
	}
	bidir, err := iperf3.ParseEnd(out.End)
	if err != nil {
		t.Fatalf("ParseEnd: %v", err)
	}
	mbps := iperf3.BitsToMbps(bidir.ToTargetBps)
	if mbps <= 0 {
		t.Errorf("BitsToMbps(ToTargetBps) should be > 0, got %f", mbps)
	}
}
