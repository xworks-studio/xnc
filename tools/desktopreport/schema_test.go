package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- Step 1 binding: the RunResult schema is EXACTLY the brief's struct.
// No json tags: the wire names are the Go field names. Locked here so any
// accidental rename/tag breaks the build, not a downstream consumer.

func TestRunResultJSONFieldNamesExact(t *testing.T) {
	want := []string{
		"Host", "GPU", "Driver", "OS", "Browser",
		"Resolution",
		"TargetFPS",
		"DurationSec",
		"OldFrameRegressions", "EpochRegressions",
		"UnrecoveredFreezes",
		"CaptureToAUP95Ms", "InputToPresentP95Ms",
		"QueueP95Ms", "QueueMaxMs", "CPUPercent", "WorkingSetMB",
		"Verdict",
	}
	got := RunResultJSONFields()
	if len(got) != len(want) {
		t.Fatalf("field count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunResultRoundTrip(t *testing.T) {
	in := RunResult{
		Host: "labs-xiaoxin", GPU: "Intel Iris Xe", Driver: "31.0.101.5333",
		OS: "Windows 11 26200", Browser: "e2eviewer",
		Resolution:          "1920x1080",
		TargetFPS:           30,
		DurationSec:         60,
		OldFrameRegressions: 2, EpochRegressions: 1, UnrecoveredFreezes: 3,
		CaptureToAUP95Ms:    12.5,
		InputToPresentP95Ms: 87.25,
		QueueP95Ms:          12.25,
		QueueMaxMs:          40.5,
		CPUPercent:          6.75,
		WorkingSetMB:        220.5,
		Verdict:             "FAIL",
	}
	jb, err := MarshalResult(&in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnmarshalResult(jb)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if in != out {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

// ---- verify: fixtures (hand-assembled JSON only ever lives in tests) ----

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

const passingFixture = `{
  "Host": "local", "GPU": "Intel hybrid", "Driver": "31.0", "OS": "Win11",
  "Browser": "e2eviewer", "Resolution": "1920x1080", "TargetFPS": 30,
  "DurationSec": 60,
  "OldFrameRegressions": 0, "EpochRegressions": 0, "UnrecoveredFreezes": 0,
  "CaptureToAUP95Ms": 12.0, "InputToPresentP95Ms": 90.0,
  "QueueP95Ms": 12.0, "QueueMaxMs": 40.0, "CPUPercent": 6.0,
  "WorkingSetMB": 220.0, "Verdict": "PASS"
}`

const failingFixture = `{
  "Host": "local", "GPU": "Intel hybrid", "Driver": "31.0", "OS": "Win11",
  "Browser": "e2eviewer", "Resolution": "1920x1080", "TargetFPS": 30,
  "DurationSec": 60,
  "OldFrameRegressions": 2, "EpochRegressions": 1, "UnrecoveredFreezes": 3,
  "CaptureToAUP95Ms": 16.5, "InputToPresentP95Ms": 187.5,
  "QueueP95Ms": 52.25, "QueueMaxMs": 120.5, "CPUPercent": 17.5,
  "WorkingSetMB": 380.25, "Verdict": "FAIL"
}`

func TestVerifyPassingFixture(t *testing.T) {
	res, err := LoadResult(writeTemp(t, "pass.json", passingFixture))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	vs := CheckResult(&res, DefaultGates())
	if len(vs) != 0 {
		t.Fatalf("passing fixture produced violations: %v", vs)
	}
}

func TestVerifyFailingFixtureAllRules(t *testing.T) {
	res, err := LoadResult(writeTemp(t, "fail.json", failingFixture))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	vs := CheckResult(&res, DefaultGates())
	if len(vs) != 9 {
		t.Fatalf("got %d violations, want 9 (3 P0 + 6 gates): %v", len(vs), vs)
	}
	byField := map[string]Violation{}
	for _, v := range vs {
		byField[v.Field] = v
	}
	for _, f := range []string{"OldFrameRegressions", "EpochRegressions", "UnrecoveredFreezes"} {
		v, ok := byField[f]
		if !ok {
			t.Errorf("missing P0 violation for %s", f)
			continue
		}
		if !v.P0 {
			t.Errorf("%s violation not marked P0", f)
		}
		if !strings.Contains(v.String(), "P0 counter must be zero") {
			t.Errorf("%s violation text = %q, want P0 wording", f, v.String())
		}
	}
	gates := map[string]float64{
		"CaptureToAUP95Ms":    15,
		"InputToPresentP95Ms": 150,
		"QueueP95Ms":          50,
		"QueueMaxMs":          100,
		"CPUPercent":          15,
		"WorkingSetMB":        350,
	}
	for f, limit := range gates {
		v, ok := byField[f]
		if !ok {
			t.Errorf("missing gate violation for %s", f)
			continue
		}
		if v.P0 {
			t.Errorf("%s gate violation wrongly marked P0", f)
		}
		if v.Limit != limit {
			t.Errorf("%s gate limit = %v, want %v", f, v.Limit, limit)
		}
	}
}

func TestVerifyGateBoundariesExclusive(t *testing.T) {
	// Exactly at every gate limit passes; limits are exclusive upper bounds.
	res, err := LoadResult(writeTemp(t, "edge.json", `{
	  "Host":"h","CaptureToAUP95Ms":15,"InputToPresentP95Ms":150,
	  "QueueP95Ms":50,"QueueMaxMs":100,"CPUPercent":15,"WorkingSetMB":350,
	  "Verdict":"PASS"}`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if vs := CheckResult(&res, DefaultGates()); len(vs) != 0 {
		t.Errorf("at-limit values must pass, got %v", vs)
	}
}

func TestLoadGatesOverride(t *testing.T) {
	p := writeTemp(t, "gates.json", `{"QueueMaxMs": 200, "CPUPercent": 60.5}`)
	g, err := LoadGates(p)
	if err != nil {
		t.Fatalf("load gates: %v", err)
	}
	if g.QueueMaxMs != 200 {
		t.Errorf("QueueMaxMs override = %v, want 200", g.QueueMaxMs)
	}
	if g.CPUPercent != 60.5 {
		t.Errorf("CPUPercent override = %v, want 60.5", g.CPUPercent)
	}
	// Unspecified gates keep the defaults.
	d := DefaultGates()
	if g.CaptureToAUP95Ms != d.CaptureToAUP95Ms || g.QueueP95Ms != d.QueueP95Ms ||
		g.WorkingSetMB != d.WorkingSetMB || g.InputToPresentP95Ms != d.InputToPresentP95Ms {
		t.Errorf("override leaked into unspecified gates: %+v", g)
	}
}

func TestLoadGatesRejectsBadJSON(t *testing.T) {
	if _, err := LoadGates(writeTemp(t, "bad.json", `{"QueueMaxMs": `)); err == nil {
		t.Error("bad JSON must error")
	}
	if _, err := LoadGates(writeTemp(t, "wrong.json", `{"Nope": 1}`)); err == nil {
		t.Error("unknown gate key must error (typos must not pass silently)")
	}
}

func TestLoadResultRejectsBadJSON(t *testing.T) {
	if _, err := LoadResult(writeTemp(t, "bad.json", `{`)); err == nil {
		t.Error("bad RunResult JSON must error")
	}
}

func TestVerifyCommandExitCodes(t *testing.T) {
	pass := writeTemp(t, "pass.json", passingFixture)
	fail := writeTemp(t, "fail.json", failingFixture)
	if code := run([]string{"verify", pass}); code != 0 {
		t.Errorf("verify passing = %d, want 0", code)
	}
	if code := run([]string{"verify", pass, fail}); code != 1 {
		t.Errorf("verify pass+fail = %d, want 1", code)
	}
	if code := run([]string{"verify", fail}); code != 1 {
		t.Errorf("verify failing = %d, want 1", code)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if code := run([]string{"verify", missing}); code != 2 {
		t.Errorf("verify missing file = %d, want 2", code)
	}
	if code := run([]string{"verify"}); code != 2 {
		t.Errorf("verify no args = %d, want 2", code)
	}
}

const noEvidenceFixture = `{
  "Host": "local", "GPU": "Intel hybrid", "Driver": "31.0", "OS": "Win11",
  "Browser": "e2eviewer", "Resolution": "1920x1080", "TargetFPS": 30,
  "DurationSec": 60,
  "OldFrameRegressions": 0, "EpochRegressions": 0, "UnrecoveredFreezes": 0,
  "CaptureToAUP95Ms": 0, "InputToPresentP95Ms": 0,
  "QueueP95Ms": 0, "QueueMaxMs": 0, "CPUPercent": 0,
  "WorkingSetMB": 0, "Verdict": "NO-EVIDENCE"
}`

func TestVerifyNoEvidenceVerdictExits1(t *testing.T) {
	// Every number is zero because nothing was measured, not because zero
	// was observed. Only the NO-EVIDENCE verdict distinguishes it from a
	// true all-zero PASS - verify must fail it (fail closed, review finding
	// Important 1).
	p := writeTemp(t, "noevidence.json", noEvidenceFixture)
	if code := run([]string{"verify", p}); code != 1 {
		t.Errorf("verify NO-EVIDENCE = %d, want 1", code)
	}
	// Not gate-relaxable either: gates cannot manufacture evidence.
	relaxed := writeTemp(t, "gates.json", `{"QueueP95Ms":1000,"CPUPercent":100}`)
	if code := run([]string{"verify", "-gates", relaxed, p}); code != 1 {
		t.Errorf("verify NO-EVIDENCE with relaxed gates = %d, want 1", code)
	}
}

func TestVerifyCommandGatesFlag(t *testing.T) {
	fail := writeTemp(t, "fail.json", failingFixture)
	relaxed := writeTemp(t, "gates.json",
		`{"QueueMaxMs":200,"CPUPercent":60,"WorkingSetMB":400,"QueueP95Ms":60,
		  "CaptureToAUP95Ms":20,"InputToPresentP95Ms":200}`)
	// P0 counters are still nonzero -> still exit 1 despite relaxed gates.
	if code := run([]string{"verify", "-gates", relaxed, fail}); code != 1 {
		t.Errorf("verify relaxed gates on P0-failing fixture = %d, want 1", code)
	}
	// Zero out the P0 counters; relaxed gates now pass what defaults failed.
	relaxedPass := strings.Replace(failingFixture,
		`"OldFrameRegressions": 2, "EpochRegressions": 1, "UnrecoveredFreezes": 3,`,
		`"OldFrameRegressions": 0, "EpochRegressions": 0, "UnrecoveredFreezes": 0,`, 1)
	p := writeTemp(t, "gateonly.json", relaxedPass)
	if code := run([]string{"verify", "-gates", relaxed, p}); code != 0 {
		t.Errorf("verify relaxed gates on gate-only fixture = %d, want 0", code)
	}
	if code := run([]string{"verify", p}); code != 1 {
		t.Errorf("verify default gates on gate-only fixture = %d, want 1", code)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	cases := []struct {
		in   []float64
		p    float64
		want float64
	}{
		{[]float64{4.2, 5.1, 6.0}, 0.95, 6.0}, // ceil(0.95*3)=3 -> max
		{[]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.95, 10},
		{[]float64{10, 2, 8, 4}, 0.50, 4}, // unsorted input; ceil(.5*4)=2 -> 4
		{[]float64{3.5}, 0.95, 3.5},
		{nil, 0.95, 0},
	}
	for _, c := range cases {
		if got := Percentile(c.in, c.p); got != c.want {
			t.Errorf("Percentile(%v, %v) = %v, want %v", c.in, c.p, got, c.want)
		}
	}
}
