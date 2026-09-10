package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// merge: one deterministic Markdown table from N results, rows in argument
// order, exact column set = the schema fields.

func TestMergeTableExact(t *testing.T) {
	a := RunResult{
		Host: "local", GPU: "Intel hybrid", Driver: "31.0.101.5333",
		OS: "Windows 11 26200", Browser: "e2eviewer", Resolution: "1920x1080",
		TargetFPS: 30, DurationSec: 60,
		CaptureToAUP95Ms: 12.5, QueueP95Ms: 12.25, QueueMaxMs: 40.5,
		CPUPercent: 6.75, WorkingSetMB: 220.25, Verdict: "PASS",
	}
	b := RunResult{
		Host: "labs-xiaoxin", GPU: "Intel UHD", Driver: "31.0.100",
		OS: "Windows 10 19045", Browser: "n/a", Resolution: "2560x1440",
		TargetFPS: 60, DurationSec: 120,
		OldFrameRegressions: 2, EpochRegressions: 1, UnrecoveredFreezes: 3,
		CaptureToAUP95Ms: 16, InputToPresentP95Ms: 187.5, QueueP95Ms: 52.25,
		QueueMaxMs: 120.5, CPUPercent: 17.5, WorkingSetMB: 380.25,
		Verdict: "FAIL",
	}
	want := `| Host | GPU | Driver | OS | Browser | Resolution | TargetFPS | DurationSec | OldFrameRegressions | EpochRegressions | UnrecoveredFreezes | CaptureToAUP95Ms | InputToPresentP95Ms | QueueP95Ms | QueueMaxMs | CPUPercent | WorkingSetMB | Verdict |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| local | Intel hybrid | 31.0.101.5333 | Windows 11 26200 | e2eviewer | 1920x1080 | 30 | 60 | 0 | 0 | 0 | 12.5 | 0.0 | 12.2 | 40.5 | 6.8 | 220.2 | PASS |
| labs-xiaoxin | Intel UHD | 31.0.100 | Windows 10 19045 | n/a | 2560x1440 | 60 | 120 | 2 | 1 | 3 | 16.0 | 187.5 | 52.2 | 120.5 | 17.5 | 380.2 | FAIL |
`
	if got := MergeTable([]RunResult{a, b}); got != want {
		t.Errorf("MergeTable mismatch:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestMergeTableEmpty(t *testing.T) {
	if got := MergeTable(nil); got != "" {
		t.Errorf("MergeTable(nil) = %q, want empty", got)
	}
}

func TestMergeCommandWritesFile(t *testing.T) {
	dir := t.TempDir()
	a := writeTemp(t, "a.json", passingFixture)
	b := writeTemp(t, "b.json", failingFixture)
	out := filepath.Join(dir, "table.md")
	if code := run([]string{"merge", "-out", out, a, b}); code != 0 {
		t.Fatalf("merge exit = %d, want 0", code)
	}
	jb, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read merged table: %v", err)
	}
	s := string(jb)
	if len(s) == 0 || s[0] != '|' {
		t.Fatalf("merged table does not start with a table row: %q", s)
	}
	// header + separator + one row per result
	if got := strings.Count(strings.TrimRight(s, "\n"), "\n") + 1; got != 4 {
		t.Errorf("merged table has %d lines, want 4:\n%s", got, s)
	}
	if !strings.Contains(s, "| FAIL |") {
		t.Error("merged table lost the FAIL verdict row")
	}
}

func TestMergeCommandBadInputExits2(t *testing.T) {
	if code := run([]string{"merge"}); code != 2 {
		t.Errorf("merge no args = %d, want 2", code)
	}
	if code := run([]string{"merge", filepath.Join(t.TempDir(), "nope.json")}); code != 2 {
		t.Errorf("merge missing file = %d, want 2", code)
	}
}
