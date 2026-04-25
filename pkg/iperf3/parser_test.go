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

func TestParseEnd_AllFourFields(t *testing.T) {
	out, err := iperf3.Parse([]byte(successJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	bidir, err := iperf3.ParseEnd(out.End)
	if err != nil {
		t.Fatalf("ParseEnd returned error: %v", err)
	}

	// Values are taken directly from successJSON above.
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"FwdSentBps", bidir.FwdSentBps, 914346934.0980172},
		{"FwdRecvBps", bidir.FwdRecvBps, 909898725.866858},
		{"RevSentBps", bidir.RevSentBps, 930914229.4635547},
		{"RevRecvBps", bidir.RevRecvBps, 926151052.768152},
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

func TestParseEnd_MissingBidirReverseFields(t *testing.T) {
	// Simulates a unidirectional test (no --bidir) or an older iperf3 build
	// where the reverse fields are absent. Rev values should be 0, not an error.
	endRaw := json.RawMessage(`{
		"sum_sent":     {"bits_per_second": 1000000000.0},
		"sum_received": {"bits_per_second":  950000000.0}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd should not error on missing reverse fields: %v", err)
	}
	if bidir.FwdSentBps != 1000000000.0 {
		t.Errorf("FwdSentBps: got %f, want 1000000000.0", bidir.FwdSentBps)
	}
	if bidir.FwdRecvBps != 950000000.0 {
		t.Errorf("FwdRecvBps: got %f, want 950000000.0", bidir.FwdRecvBps)
	}
	if bidir.RevSentBps != 0 {
		t.Errorf("RevSentBps: got %f, want 0 (field absent)", bidir.RevSentBps)
	}
	if bidir.RevRecvBps != 0 {
		t.Errorf("RevRecvBps: got %f, want 0 (field absent)", bidir.RevRecvBps)
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
	// Extra unknown fields must not cause a failure.
	endRaw := json.RawMessage(`{
		"sum_sent": {"bits_per_second": 500000000.0, "unknown_future_field": true},
		"sum_received": {"bits_per_second": 480000000.0},
		"sum_sent_bidir_reverse": {"bits_per_second": 510000000.0},
		"sum_received_bidir_reverse": {"bits_per_second": 495000000.0},
		"new_section_we_dont_know_about": {"foo": "bar"}
	}`)
	bidir, err := iperf3.ParseEnd(endRaw)
	if err != nil {
		t.Fatalf("ParseEnd should tolerate unknown fields: %v", err)
	}
	if bidir.FwdSentBps <= 0 {
		t.Errorf("FwdSentBps should be > 0, got %f", bidir.FwdSentBps)
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
	mbps := iperf3.BitsToMbps(bidir.FwdSentBps)
	if mbps <= 0 {
		t.Errorf("BitsToMbps(FwdSentBps) should be > 0, got %f", mbps)
	}
}
