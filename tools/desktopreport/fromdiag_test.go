package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// from-diag: the ONLY production path into RunResult (ruling 3). It reads
// the real tool outputs: the native stats.json sidecar, e2eviewer summary
// JSON, collect-metrics JSONL, optional browser-gate metrics. Fixtures below
// mirror the exact emitter shapes (native/desktop/pipeline.cpp FormatStatsJson
// + FormatStagesJson; tools/e2eviewer summary; scripts/desktop-media/
// collect-metrics.ps1).

const diagStatsV2Fixture = `{
  "duration_s": 60,
  "width": 1920,
  "height": 1080,
  "fps": 30,
  "bitrate_bps": 6000000,
  "captured": 1798, "encoded": 1798, "keyframes": 2, "timeouts": 0,
  "warmup_feeds": 1, "rebuilds": 0, "resets": 0,
  "aus_written": 1798, "bytes_written": 12345678,
  "stages": {
    "stage_semantics": "loop-thread wall time in us; gpu_copy_us is wait-inclusive",
    "gpu_copy_us": { "n": 1798, "p50": 3000, "p95": 8000, "p99": 9000 },
    "gpu_convert_us": { "n": 1798, "p50": 500, "p95": 900, "p99": 1000 },
    "mft_submit_to_output_us": { "n": 1798, "p50": 4000, "p95": 11000, "p99": 13000 },
    "inflight_slots": { "n": 1798, "p50": 1, "p95": 2, "p99": 3 },
    "queue_age_us": { "n": 1798, "p50": 300, "p95": 1200, "p99": 2000 },
    "capture_to_au_us": { "n": 1798, "p50": 6000, "p95": 12000, "p99": 15000 }
  },
  "cpu_readbacks": 0,
  "encoder_backend": "software",
  "ok": 1
}`

// The M0/v1 sidecar has NO stages block at all.
const diagStatsV1Fixture = `{
  "duration_s": 30,
  "width": 1280, "height": 720, "fps": 15, "bitrate_bps": 2000000,
  "captured": 448, "encoded": 448, "keyframes": 1, "timeouts": 0,
  "warmup_feeds": 1, "rebuilds": 0, "resets": 0,
  "aus_written": 448, "bytes_written": 234567,
  "ok": 1
}`

const e2ePassFixture = `{"mode":"direct","connected":true,"firstFrameMs":120,
  "frames":1790,"keyframes":2,"bytes":12300000,"plisSent":1,"pliToIdrMaxMs":180,
  "queueAgeP50Ms":3.5,"queueAgeP95Ms":12.25,"queueAgeMaxMs":40.5,
  "rtpTsRegressions":0,"frameMetaCount":1790,
  "contentIdRegressions":0,"encodeSeqRegressions":0,"codecEpochRegressions":0,
  "recoveryViolations":0,"pausedEvents":0,"pausedMs":0,
  "durationMs":60000,"assertionsPassed":true}`

const e2eFailFixture = `{"mode":"direct","connected":true,"firstFrameMs":150,
  "frames":1780,"keyframes":2,"bytes":12200000,"plisSent":2,"pliToIdrMaxMs":210,
  "queueAgeP50Ms":8.0,"queueAgeP95Ms":52.25,"queueAgeMaxMs":120.5,
  "rtpTsRegressions":1,"frameMetaCount":1780,
  "contentIdRegressions":2,"encodeSeqRegressions":1,"codecEpochRegressions":1,
  "recoveryViolations":3,"pausedEvents":1,"pausedMs":900,
  "durationMs":60000,"assertionsPassed":false,"failures":["queue hard max"]}`

const metricsFixture = `{"ts":"2026-08-26T10:00:00Z","pid":4242,"cpuPercent":4.2,"workingSetMB":210.5}
{"ts":"2026-08-26T10:00:01Z","pid":4242,"cpuPercent":5.1,"workingSetMB":215.0}
{"ts":"2026-08-26T10:00:02Z","pid":4242,"cpuPercent":6.0,"workingSetMB":220.25}
`

// A written-but-disconnected viewer summary (dial failure that still printed
// its JSON before exiting 1): non-empty file, zero receive-side evidence.
const e2eDialFailureFixture = `{"mode":"direct","connected":false,"firstFrameMs":0,
  "frames":0,"keyframes":0,"bytes":0,"plisSent":0,"pliToIdrMaxMs":0,
  "queueAgeP50Ms":0,"queueAgeP95Ms":0,"queueAgeMaxMs":0,
  "rtpTsRegressions":0,"frameMetaCount":0,"contentIdRegressions":0,
  "encodeSeqRegressions":0,"codecEpochRegressions":0,"recoveryViolations":0,
  "pausedEvents":0,"pausedMs":0,"durationMs":30000,"assertionsPassed":false,
  "failures":["viewer not connected after 25s (state=failed)"]}`

const browserMetricsFixture = `{"browser":"Chrome 126","inputToPresentP95Ms":87.5}`

func statsPath(t *testing.T, stats string) string {
	t.Helper()
	return writeTemp(t, "stats.json", stats)
}

func TestFromDiagFullMapping(t *testing.T) {
	res, err := FromDiag(FromDiagInput{
		StatsPath:   statsPath(t, diagStatsV2Fixture),
		E2EPaths:    []string{writeTemp(t, "viewer.json", e2ePassFixture)},
		MetricsPath: writeTemp(t, "metrics.jsonl", metricsFixture),
		BrowserPath: writeTemp(t, "browser.json", browserMetricsFixture),
		Meta: MetaOverrides{
			Host: "local", GPU: "Intel hybrid", Driver: "31.0.101.5333",
			OS: "Windows 11 26200",
		},
		Gates: DefaultGates(),
	})
	if err != nil {
		t.Fatalf("FromDiag: %v", err)
	}
	// From stats.json:
	if res.Resolution != "1920x1080" {
		t.Errorf("Resolution = %q, want 1920x1080", res.Resolution)
	}
	if res.TargetFPS != 30 || res.DurationSec != 60 {
		t.Errorf("TargetFPS/DurationSec = %d/%d, want 30/60", res.TargetFPS, res.DurationSec)
	}
	if res.CaptureToAUP95Ms != 12.0 {
		t.Errorf("CaptureToAUP95Ms = %v, want 12 (12000us/1000)", res.CaptureToAUP95Ms)
	}
	// From e2eviewer:
	if res.QueueP95Ms != 12.25 || res.QueueMaxMs != 40.5 {
		t.Errorf("Queue = %v/%v, want 12.25/40.5", res.QueueP95Ms, res.QueueMaxMs)
	}
	if res.OldFrameRegressions != 0 || res.EpochRegressions != 0 || res.UnrecoveredFreezes != 0 {
		t.Errorf("P0 counters = %d/%d/%d, want 0/0/0", res.OldFrameRegressions,
			res.EpochRegressions, res.UnrecoveredFreezes)
	}
	// From collect-metrics JSONL (p95 CPU of 3 samples = max; max WS):
	if res.CPUPercent != 6.0 {
		t.Errorf("CPUPercent = %v, want 6.0 (p95)", res.CPUPercent)
	}
	if res.WorkingSetMB != 220.25 {
		t.Errorf("WorkingSetMB = %v, want 220.25 (max)", res.WorkingSetMB)
	}
	// From browser metrics:
	if res.InputToPresentP95Ms != 87.5 {
		t.Errorf("InputToPresentP95Ms = %v, want 87.5", res.InputToPresentP95Ms)
	}
	if res.Browser != "Chrome 126" {
		t.Errorf("Browser = %q, want Chrome 126", res.Browser)
	}
	// Meta flags:
	if res.Host != "local" || res.GPU != "Intel hybrid" {
		t.Errorf("meta lost: %+v", res)
	}
	if res.Verdict != "PASS" {
		t.Errorf("Verdict = %q, want PASS", res.Verdict)
	}
}

func TestFromDiagRegressionsMapping(t *testing.T) {
	// OldFrameRegressions = contentId + rtpTs + encodeSeq regressions;
	// EpochRegressions = codecEpoch; UnrecoveredFreezes = recoveryViolations.
	// Complete fixture (stats + e2e + metrics) so the verdict reflects the
	// violations, not missing evidence.
	res, err := FromDiag(FromDiagInput{
		StatsPath:   statsPath(t, diagStatsV2Fixture),
		E2EPaths:    []string{writeTemp(t, "viewer.json", e2eFailFixture)},
		MetricsPath: writeTemp(t, "metrics.jsonl", metricsFixture),
		Gates:       DefaultGates(),
	})
	if err != nil {
		t.Fatalf("FromDiag: %v", err)
	}
	if res.OldFrameRegressions != 4 { // 2 contentId + 1 rtpTs + 1 encodeSeq
		t.Errorf("OldFrameRegressions = %d, want 4", res.OldFrameRegressions)
	}
	if res.EpochRegressions != 1 {
		t.Errorf("EpochRegressions = %d, want 1", res.EpochRegressions)
	}
	if res.UnrecoveredFreezes != 3 {
		t.Errorf("UnrecoveredFreezes = %d, want 3", res.UnrecoveredFreezes)
	}
	if res.QueueP95Ms != 52.25 || res.QueueMaxMs != 120.5 {
		t.Errorf("Queue = %v/%v, want 52.25/120.5", res.QueueP95Ms, res.QueueMaxMs)
	}
	if res.Verdict != "FAIL" {
		t.Errorf("Verdict = %q, want FAIL", res.Verdict)
	}
	vs := CheckResult(&res, DefaultGates())
	if len(vs) != 5 { // 3 P0 + QueueP95 + QueueMax
		t.Errorf("violations = %d, want 5: %v", len(vs), vs)
	}
}

func TestFromDiagMultipleViewersMergeWorst(t *testing.T) {
	res, err := FromDiag(FromDiagInput{
		StatsPath: statsPath(t, diagStatsV2Fixture),
		E2EPaths: []string{
			writeTemp(t, "v1.json", e2ePassFixture),
			writeTemp(t, "v2.json", e2eFailFixture),
		},
		Gates: DefaultGates(),
	})
	if err != nil {
		t.Fatalf("FromDiag: %v", err)
	}
	// Queue percentiles: WORST viewer wins (max).
	if res.QueueP95Ms != 52.25 || res.QueueMaxMs != 120.5 {
		t.Errorf("Queue = %v/%v, want worst-viewer 52.25/120.5", res.QueueP95Ms, res.QueueMaxMs)
	}
	// Regression counters accumulate across every viewer (all evidence).
	if res.OldFrameRegressions != 4 || res.EpochRegressions != 1 || res.UnrecoveredFreezes != 3 {
		t.Errorf("P0 = %d/%d/%d, want 4/1/3", res.OldFrameRegressions,
			res.EpochRegressions, res.UnrecoveredFreezes)
	}
}

func TestFromDiagV1SidecarNoStages(t *testing.T) {
	// A v1 (M0) sidecar carries no stages: CaptureToAUP95Ms stays 0, the
	// resolution/fps/duration still map, and the absent stage makes the run
	// NO-EVIDENCE (a v1 soak cannot evidence the capture-to-AU gate).
	res, missing, err := FromDiagDetailed(FromDiagInput{
		StatsPath: statsPath(t, diagStatsV1Fixture),
		Gates:     DefaultGates(),
	})
	if err != nil {
		t.Fatalf("FromDiag: %v", err)
	}
	if res.CaptureToAUP95Ms != 0 {
		t.Errorf("CaptureToAUP95Ms = %v, want 0 (no stages in v1 sidecar)", res.CaptureToAUP95Ms)
	}
	if res.Resolution != "1280x720" || res.TargetFPS != 15 || res.DurationSec != 30 {
		t.Errorf("v1 mapping wrong: %+v", res)
	}
	if res.Verdict != VerdictNoEvidence {
		t.Errorf("Verdict = %q, want NO-EVIDENCE (capture stage absent)", res.Verdict)
	}
	if !containsStr(missing, "CaptureToAUP95Ms") {
		t.Errorf("missing list = %v, want CaptureToAUP95Ms", missing)
	}
}

func TestFromDiagCommandEndToEnd(t *testing.T) {
	stats := statsPath(t, diagStatsV2Fixture)
	e2e := writeTemp(t, "viewer.json", e2eFailFixture)
	metrics := writeTemp(t, "metrics.jsonl", metricsFixture)
	out := writeTemp(t, "runresult.json", "")
	code := run([]string{
		"from-diag", "-stats", stats, "-e2e", e2e, "-metrics", metrics,
		"-host", "box-a", "-gpu", "Intel Iris", "-browser", "e2eviewer",
		"-out", out,
	})
	if code != 0 {
		t.Fatalf("from-diag exit = %d, want 0", code)
	}
	res, err := LoadResult(out)
	if err != nil {
		t.Fatalf("load emitted runresult: %v", err)
	}
	if res.Host != "box-a" || res.Browser != "e2eviewer" || res.GPU != "Intel Iris" {
		t.Errorf("meta flags lost: %+v", res)
	}
	if res.Verdict != "FAIL" {
		t.Errorf("Verdict = %q, want FAIL", res.Verdict)
	}
	// The emitted JSON must be valid RunResult wire form (exact field set).
	jb := readFileForTest(t, out)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jb), &probe); err != nil {
		t.Fatalf("emitted JSON invalid: %v", err)
	}
	if len(probe) != len(RunResultJSONFields()) {
		t.Errorf("emitted JSON has %d fields, want %d", len(probe), len(RunResultJSONFields()))
	}
}

func TestFromDiagMissingStatsExits2(t *testing.T) {
	missing := writeTemp(t, "stats-missing.json", "")
	code := run([]string{"from-diag", "-stats", missing})
	if code != 2 {
		t.Errorf("from-diag missing stats = %d, want 2", code)
	}
	code = run([]string{"from-diag"})
	if code != 2 {
		t.Errorf("from-diag no args = %d, want 2", code)
	}
}

func TestFromDiagGatesOverrideFlipsVerdict(t *testing.T) {
	stats := statsPath(t, diagStatsV2Fixture)
	e2e := writeTemp(t, "viewer.json", e2eFailFixture)
	metrics := writeTemp(t, "metrics.jsonl", metricsFixture)
	relaxed := writeTemp(t, "gates.json", `{"QueueP95Ms":60,"QueueMaxMs":200}`)
	out := writeTemp(t, "runresult.json", "")
	if code := run([]string{"from-diag", "-stats", stats, "-e2e", e2e,
		"-metrics", metrics, "-gates", relaxed, "-out", out}); code != 0 {
		t.Fatalf("from-diag exit = %d, want 0", code)
	}
	res, err := LoadResult(out)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Gates relaxed but P0 counters still nonzero -> verdict stays FAIL.
	if res.Verdict != "FAIL" {
		t.Errorf("Verdict = %q, want FAIL (P0 counters are not gate-relaxable)", res.Verdict)
	}
}

// ---- fail closed: absent mandatory evidence must read NO-EVIDENCE, not PASS ----
// (Review finding Important 1: missing receive-side evidence used to leave
// QueueP95/QueueMax + the three P0 counters - and, for empty inputs, CPU/
// WorkingSet and CaptureToAUP95 - at a passing zero.)

func TestFromDiagNoViewerEvidenceIsNoEvidence(t *testing.T) {
	// No -e2e at all: Queue metrics + the three P0 counters have no evidence.
	res, missing, err := FromDiagDetailed(FromDiagInput{
		StatsPath:   statsPath(t, diagStatsV2Fixture),
		MetricsPath: writeTemp(t, "metrics.jsonl", metricsFixture),
		Gates:       DefaultGates(),
	})
	if err != nil {
		t.Fatalf("FromDiag: %v", err)
	}
	if res.Verdict != VerdictNoEvidence {
		t.Errorf("Verdict = %q, want NO-EVIDENCE (no viewer reports supplied)", res.Verdict)
	}
	for _, want := range []string{"QueueP95Ms", "QueueMaxMs",
		"OldFrameRegressions", "EpochRegressions", "UnrecoveredFreezes"} {
		if !containsStr(missing, want) {
			t.Errorf("missing list %v lacks %s", missing, want)
		}
	}
	if containsStr(missing, "CPUPercent") || containsStr(missing, "CaptureToAUP95Ms") {
		t.Errorf("metrics/stage evidence WAS supplied but flagged missing: %v", missing)
	}

	// A written-but-disconnected viewer summary (frames=0, e.g. a dial
	// failure that still printed its JSON) is equally no evidence.
	res, missing, err = FromDiagDetailed(FromDiagInput{
		StatsPath:   statsPath(t, diagStatsV2Fixture),
		E2EPaths:    []string{writeTemp(t, "dial.json", e2eDialFailureFixture)},
		MetricsPath: writeTemp(t, "metrics.jsonl", metricsFixture),
		Gates:       DefaultGates(),
	})
	if err != nil {
		t.Fatalf("FromDiag dial-failure: %v", err)
	}
	if res.Verdict != VerdictNoEvidence {
		t.Errorf("Verdict = %q, want NO-EVIDENCE (viewer decoded zero frames)", res.Verdict)
	}
	if !containsStr(missing, "QueueP95Ms") {
		t.Errorf("missing list %v lacks QueueP95Ms", missing)
	}
}

func TestFromDiagAbsentStageAndEmptyMetricsIsNoEvidence(t *testing.T) {
	// v1 sidecar (no stages block; the native emitter omits zero-sample
	// stages - absent, not zero) + an EMPTY metrics file: CaptureToAUP95Ms,
	// CPUPercent and WorkingSetMB have no evidence.
	res, missing, err := FromDiagDetailed(FromDiagInput{
		StatsPath:   statsPath(t, diagStatsV1Fixture),
		E2EPaths:    []string{writeTemp(t, "viewer.json", e2ePassFixture)},
		MetricsPath: writeTemp(t, "metrics.jsonl", ""),
		Gates:       DefaultGates(),
	})
	if err != nil {
		t.Fatalf("FromDiag: %v", err)
	}
	if res.CPUPercent != 0 || res.WorkingSetMB != 0 || res.CaptureToAUP95Ms != 0 {
		t.Errorf("absent metrics must stay zero, got %+v", res)
	}
	if res.Verdict != VerdictNoEvidence {
		t.Errorf("Verdict = %q, want NO-EVIDENCE (stage absent, zero metric samples)", res.Verdict)
	}
	for _, want := range []string{"CaptureToAUP95Ms", "CPUPercent", "WorkingSetMB"} {
		if !containsStr(missing, want) {
			t.Errorf("missing list %v lacks %s", missing, want)
		}
	}

	// A present-but-zero-sample stage entry (n=0) is likewise absent: the
	// emitter omits zero-sample stages by contract, and a defensive n=0
	// read must not count as evidence either.
	zeroSample := `{"duration_s":30,"width":1280,"height":720,"fps":15,
	  "stages":{"capture_to_au_us":{"n":0,"p50":0,"p95":0,"p99":0}},"ok":1}`
	var ms float64
	var ok bool
	var st diagStats
	if err := json.Unmarshal([]byte(zeroSample), &st); err != nil {
		t.Fatalf("unmarshal zero-sample fixture: %v", err)
	}
	if ms, ok = st.stageP95Ms("capture_to_au_us"); ok || ms != 0 {
		t.Errorf("zero-sample stage = (%v, %v), want (0, false)", ms, ok)
	}
}

func TestFromDiagNoEvidenceFailsVerifyEndToEnd(t *testing.T) {
	statsV2 := statsPath(t, diagStatsV2Fixture)
	statsV1 := statsPath(t, diagStatsV1Fixture)
	e2e := writeTemp(t, "viewer.json", e2ePassFixture)
	metrics := writeTemp(t, "metrics.jsonl", metricsFixture)
	emptyMetrics := writeTemp(t, "metrics-empty.jsonl", "")

	// 1. No viewer evidence: from-diag emits a result, verify exits nonzero.
	out := writeTemp(t, "runresult-noe2e.json", "")
	if code := run([]string{"from-diag", "-stats", statsV2, "-metrics", metrics, "-out", out}); code != 0 {
		t.Fatalf("from-diag (no -e2e) exit = %d, want 0 (it still emits a result)", code)
	}
	res, err := LoadResult(out)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res.Verdict != "NO-EVIDENCE" {
		t.Errorf("Verdict = %q, want NO-EVIDENCE", res.Verdict)
	}
	if code := run([]string{"verify", out}); code != 1 {
		t.Errorf("verify no-e2e result = %d, want 1", code)
	}

	// 2. Absent stage + empty metrics through the same chain.
	out2 := writeTemp(t, "runresult-nostage.json", "")
	if code := run([]string{"from-diag", "-stats", statsV1, "-e2e", e2e, "-metrics", emptyMetrics, "-out", out2}); code != 0 {
		t.Fatalf("from-diag (v1 + empty metrics) exit = %d, want 0", code)
	}
	if code := run([]string{"verify", out2}); code != 1 {
		t.Errorf("verify absent-stage/empty-metrics result = %d, want 1", code)
	}

	// 3. The complete fixture (stats + e2e + metrics) still passes.
	out3 := writeTemp(t, "runresult-complete.json", "")
	if code := run([]string{"from-diag", "-stats", statsV2, "-e2e", e2e, "-metrics", metrics, "-out", out3}); code != 0 {
		t.Fatalf("from-diag (complete) exit = %d, want 0", code)
	}
	if code := run([]string{"verify", out3}); code != 0 {
		t.Errorf("verify complete fixture = %d, want 0", code)
	}
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	jb, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(jb))
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
