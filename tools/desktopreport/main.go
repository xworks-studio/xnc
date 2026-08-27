// desktopreport - M4 Task 1: RunResult producer/verifier/merger.
//
// Subcommands:
//
//	desktopreport verify  [-gates gates.json] result.json [result2.json ...]
//	    Exit 0 = every result passes (all P0 counters zero + no gate
//	    exceeded); exit 1 = any rule failed; exit 2 = usage/IO error.
//	    The P0 counters (OldFrameRegressions, EpochRegressions,
//	    UnrecoveredFreezes) must be zero and are NOT gate-relaxable.
//
//	desktopreport merge   [-out table.md] result.json ...
//	    One deterministic Markdown table from N results (stdout or -out).
//
//	desktopreport from-diag -stats stats.json [-e2e viewer.json]...
//	                          [-metrics metrics.jsonl] [-browser-metrics b.json]
//	                          [-host H] [-gpu G] [-driver D] [-os O] [-browser B]
//	                          [-resolution WxH] [-target-fps N] [-duration-sec N]
//	                          [-gates gates.json] [-out result.json]
//	    The ONLY production path into RunResult (ruling 3): reads the real
//	    tool outputs - the native console-diag stats.json sidecar, e2eviewer
//	    summary JSON, collect-metrics.ps1 JSONL, optional browser-gate
//	    metrics - and computes the Verdict from the gate profile.
//
// Field mapping (documented, single place):
//
//	Resolution/TargetFPS/DurationSec <- stats.json width x height / fps /
//	  duration_s (overridable by flags; zero-value flags keep the stats).
//	CaptureToAUP95Ms  <- stages.capture_to_au_us.p95 / 1000 (0 when the
//	  v1/M0 sidecar carries no stages block).
//	QueueP95Ms/QueueMaxMs <- e2eviewer queueAgeP95Ms / queueAgeMaxMs;
//	  multiple viewers merge WORST (max) - any viewer tripping is a trip.
//	OldFrameRegressions <- sum over viewers of contentIdRegressions +
//	  rtpTsRegressions + encodeSeqRegressions (every frame-order
//	  regression is old-frame evidence; summing is fail-closed).
//	EpochRegressions    <- sum of codecEpochRegressions.
//	UnrecoveredFreezes  <- sum of recoveryViolations (post-gap recovery
//	  not starting with an IDR = freeze evidence).
//	CPUPercent          <- p95 of collect-metrics cpuPercent samples
//	  (normalized to total machine capacity, see collect-metrics.ps1).
//	WorkingSetMB        <- max of workingSetMB samples.
//	InputToPresentP95Ms + Browser <- browser-metrics JSON
//	  {"browser": "...", "inputToPresentP95Ms": N} (the Task 3 browser
//	  gates; absent = 0/"" for diag-only rows).
//
// The harness moves counters/hashes/percentiles only - never raw pixels.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches a subcommand; returned int is the process exit code.
func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "verify":
		return cmdVerify(args[1:])
	case "merge":
		return cmdMerge(args[1:])
	case "from-diag":
		return cmdFromDiag(args[1:])
	case "-h", "-help", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "desktopreport: unknown subcommand %q\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage:
  desktopreport verify  [-gates gates.json] result.json [result2.json ...]
  desktopreport merge   [-out table.md] result.json [result2.json ...]
  desktopreport from-diag -stats stats.json [-e2e viewer.json]... [-metrics m.jsonl]
                          [-browser-metrics b.json] [-host H] [-gpu G] [-driver D]
                          [-os O] [-browser B] [-resolution WxH] [-target-fps N]
                          [-duration-sec N] [-gates gates.json] [-out result.json]
`)
}

func usageErr(fs *flag.FlagSet, format string, args ...interface{}) int {
	fmt.Fprintf(os.Stderr, "desktopreport %s: %s\n%s\n", fs.Name(), fmt.Sprintf(format, args...), flagErrMsg(fs))
	return 2
}

func flagErrMsg(fs *flag.FlagSet) string {
	var b strings.Builder
	fs.SetOutput(&b)
	fs.Usage()
	return b.String()
}

// ---- verify ----

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	gatesPath := fs.String("gates", "", "gates override JSON (partial; keys = gate field names)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	files := fs.Args()
	if len(files) == 0 {
		return usageErr(fs, "no result files given")
	}
	gates := DefaultGates()
	if *gatesPath != "" {
		g, err := LoadGates(*gatesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "desktopreport verify: %v\n", err)
			return 2
		}
		gates = g
	}
	failed := 0
	for _, f := range files {
		res, err := LoadResult(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "desktopreport verify: %s: %v\n", f, err)
			return 2
		}
		vs := CheckResult(&res, gates)
		if len(vs) == 0 {
			fmt.Printf("PASS %s\n", f)
			continue
		}
		failed++
		fmt.Printf("FAIL %s\n", f)
		for _, v := range vs {
			fmt.Printf("  %s\n", v.String())
		}
	}
	if failed > 0 {
		fmt.Printf("verify: FAIL (%d of %d results failed)\n", failed, len(files))
		return 1
	}
	fmt.Printf("verify: PASS (%d results)\n", len(files))
	return 0
}

// ---- merge ----

func cmdMerge(args []string) int {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	out := fs.String("out", "", "write the table here (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	files := fs.Args()
	if len(files) == 0 {
		return usageErr(fs, "no result files given")
	}
	var results []RunResult
	for _, f := range files {
		res, err := LoadResult(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "desktopreport merge: %s: %v\n", f, err)
			return 2
		}
		results = append(results, res)
	}
	table := MergeTable(results)
	if *out == "" {
		fmt.Print(table)
		return 0
	}
	if err := os.WriteFile(*out, []byte(table), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "desktopreport merge: %v\n", err)
		return 2
	}
	fmt.Printf("merge: %d results -> %s\n", len(results), *out)
	return 0
}

// MergeTable renders one Markdown table; rows in input order. Integers
// plain, metrics one decimal, strings verbatim (no escaping needed - the
// schema fields are short identifiers).
func MergeTable(results []RunResult) string {
	if len(results) == 0 {
		return ""
	}
	headers := RunResultJSONFields()
	sep := "| " + strings.Join(repeat("---", len(headers)), " | ") + " |"
	var b strings.Builder
	b.WriteString("| " + strings.Join(headers, " | ") + " |\n")
	b.WriteString(sep + "\n")
	for i := range results {
		r := &results[i]
		cells := []string{
			r.Host, r.GPU, r.Driver, r.OS, r.Browser, r.Resolution,
			strconv.FormatUint(uint64(r.TargetFPS), 10),
			strconv.FormatUint(r.DurationSec, 10),
			strconv.FormatUint(r.OldFrameRegressions, 10),
			strconv.FormatUint(r.EpochRegressions, 10),
			strconv.FormatUint(r.UnrecoveredFreezes, 10),
			formatMs(r.CaptureToAUP95Ms),
			formatMs(r.InputToPresentP95Ms),
			formatMs(r.QueueP95Ms),
			formatMs(r.QueueMaxMs),
			formatMs(r.CPUPercent),
			formatMs(r.WorkingSetMB),
			r.Verdict,
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return b.String()
}

// ---- from-diag ----

// diagStats mirrors the native stats.json sidecar
// (native/desktop/pipeline.cpp FormatStatsJson + FormatStagesJson).
type diagStats struct {
	DurationS  uint32                     `json:"duration_s"`
	Width      uint32                     `json:"width"`
	Height     uint32                     `json:"height"`
	FPS        uint32                     `json:"fps"`
	Stages     map[string]json.RawMessage `json:"stages"`
	EncBackend string                     `json:"encoder_backend"`
}

type stagePercentiles struct {
	P95 uint64 `json:"p95"`
}

func (d *diagStats) stageP95Ms(name string) float64 {
	if d.Stages == nil {
		return 0
	}
	raw, ok := d.Stages[name]
	if !ok {
		return 0
	}
	var s stagePercentiles
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0
	}
	return float64(s.P95) / 1000.0
}

// viewerSummary mirrors the e2eviewer report fields this tool consumes
// (tools/e2eviewer main.go `summary`; unknown members ignored on decode).
type viewerSummary struct {
	QueueAgeP95Ms         float64 `json:"queueAgeP95Ms"`
	QueueAgeMaxMs         float64 `json:"queueAgeMaxMs"`
	RtpTsRegressions      int     `json:"rtpTsRegressions"`
	ContentIdRegressions  int     `json:"contentIdRegressions"`
	EncodeSeqRegressions  int     `json:"encodeSeqRegressions"`
	CodecEpochRegressions int     `json:"codecEpochRegressions"`
	RecoveryViolations    int     `json:"recoveryViolations"`
}

// metricSample mirrors one collect-metrics.ps1 JSONL line.
type metricSample struct {
	TS           string  `json:"ts"`
	Pid          int     `json:"pid"`
	CPUPercent   float64 `json:"cpuPercent"`
	WorkingSetMB float64 `json:"workingSetMB"`
}

// browserMetrics carries the Task 3 browser-gate measurements.
type browserMetrics struct {
	Browser             string  `json:"browser"`
	InputToPresentP95Ms float64 `json:"inputToPresentP95Ms"`
}

// MetaOverrides are the from-diag identity flags. Zero values keep the
// stats-derived value (or stay empty for free-text fields).
type MetaOverrides struct {
	Host, GPU, Driver, OS, Browser, Resolution string
	TargetFPS                                  uint32
	DurationSec                                uint64
}

// FromDiagInput is everything from-diag needs.
type FromDiagInput struct {
	StatsPath   string
	E2EPaths    []string
	MetricsPath string
	BrowserPath string
	Meta        MetaOverrides
	Gates       Gates
}

// FromDiag converts real tool outputs into one RunResult (see the mapping
// table in the file header).
func FromDiag(in FromDiagInput) (RunResult, error) {
	res := RunResult{}
	jb, err := os.ReadFile(in.StatsPath)
	if err != nil {
		return res, fmt.Errorf("stats: %w", err)
	}
	var stats diagStats
	if err := json.Unmarshal(jb, &stats); err != nil {
		return res, fmt.Errorf("stats %s: %w", in.StatsPath, err)
	}
	res.TargetFPS = stats.FPS
	res.DurationSec = uint64(stats.DurationS)
	if stats.Width > 0 && stats.Height > 0 {
		res.Resolution = strconv.FormatUint(uint64(stats.Width), 10) + "x" +
			strconv.FormatUint(uint64(stats.Height), 10)
	}
	res.CaptureToAUP95Ms = stats.stageP95Ms("capture_to_au_us")

	for _, p := range in.E2EPaths {
		jb, err := os.ReadFile(p)
		if err != nil {
			return res, fmt.Errorf("e2e: %w", err)
		}
		var v viewerSummary
		if err := json.Unmarshal(jb, &v); err != nil {
			return res, fmt.Errorf("e2e %s: %w", p, err)
		}
		if v.QueueAgeP95Ms > res.QueueP95Ms {
			res.QueueP95Ms = v.QueueAgeP95Ms
		}
		if v.QueueAgeMaxMs > res.QueueMaxMs {
			res.QueueMaxMs = v.QueueAgeMaxMs
		}
		res.OldFrameRegressions += uint64(v.ContentIdRegressions + v.RtpTsRegressions + v.EncodeSeqRegressions)
		res.EpochRegressions += uint64(v.CodecEpochRegressions)
		res.UnrecoveredFreezes += uint64(v.RecoveryViolations)
	}

	if in.MetricsPath != "" {
		f, err := os.Open(in.MetricsPath)
		if err != nil {
			return res, fmt.Errorf("metrics: %w", err)
		}
		defer f.Close()
		var cpus, ws []float64
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var m metricSample
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				return res, fmt.Errorf("metrics %s: bad line %q: %w", in.MetricsPath, line, err)
			}
			cpus = append(cpus, m.CPUPercent)
			ws = append(ws, m.WorkingSetMB)
		}
		if err := sc.Err(); err != nil {
			return res, fmt.Errorf("metrics %s: %w", in.MetricsPath, err)
		}
		res.CPUPercent = Percentile(cpus, 0.95)
		for _, w := range ws {
			if w > res.WorkingSetMB {
				res.WorkingSetMB = w
			}
		}
	}

	if in.BrowserPath != "" {
		jb, err := os.ReadFile(in.BrowserPath)
		if err != nil {
			return res, fmt.Errorf("browser-metrics: %w", err)
		}
		var b browserMetrics
		if err := json.Unmarshal(jb, &b); err != nil {
			return res, fmt.Errorf("browser-metrics %s: %w", in.BrowserPath, err)
		}
		res.InputToPresentP95Ms = b.InputToPresentP95Ms
		if b.Browser != "" {
			res.Browser = b.Browser
		}
	}

	// Meta overrides (flags beat derived values).
	if in.Meta.Host != "" {
		res.Host = in.Meta.Host
	}
	if in.Meta.GPU != "" {
		res.GPU = in.Meta.GPU
	}
	if in.Meta.Driver != "" {
		res.Driver = in.Meta.Driver
	}
	if in.Meta.OS != "" {
		res.OS = in.Meta.OS
	}
	if in.Meta.Browser != "" {
		res.Browser = in.Meta.Browser
	}
	if in.Meta.Resolution != "" {
		res.Resolution = in.Meta.Resolution
	}
	if in.Meta.TargetFPS != 0 {
		res.TargetFPS = in.Meta.TargetFPS
	}
	if in.Meta.DurationSec != 0 {
		res.DurationSec = in.Meta.DurationSec
	}

	if len(CheckResult(&res, in.Gates)) == 0 {
		res.Verdict = "PASS"
	} else {
		res.Verdict = "FAIL"
	}
	return res, nil
}

func cmdFromDiag(args []string) int {
	fs := flag.NewFlagSet("from-diag", flag.ExitOnError)
	stats := fs.String("stats", "", "native console-diag stats.json sidecar (required)")
	e2e := multiFlag{}
	fs.Var(&e2e, "e2e", "e2eviewer summary JSON (repeatable; merged worst/summed)")
	metrics := fs.String("metrics", "", "collect-metrics.ps1 JSONL samples")
	browser := fs.String("browser-metrics", "", "browser-gate JSON {browser, inputToPresentP95Ms}")
	gatesPath := fs.String("gates", "", "gates override JSON")
	out := fs.String("out", "", "write the RunResult JSON here (default stdout)")
	metaHost := fs.String("host", "", "host name (default: this machine)")
	metaGPU := fs.String("gpu", "", "GPU label")
	metaDriver := fs.String("driver", "", "driver version")
	metaOS := fs.String("os", "", "OS label")
	metaBrowser := fs.String("browser", "", "browser label")
	metaRes := fs.String("resolution", "", "override resolution WxH (default: from stats)")
	metaFPS := fs.Uint64("target-fps", 0, "override target fps (default: from stats)")
	metaDur := fs.Uint64("duration-sec", 0, "override duration (default: from stats)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *stats == "" {
		return usageErr(fs, "-stats is required")
	}
	gates := DefaultGates()
	if *gatesPath != "" {
		g, err := LoadGates(*gatesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "desktopreport from-diag: %v\n", err)
			return 2
		}
		gates = g
	}
	host := *metaHost
	if host == "" {
		if hn, err := os.Hostname(); err == nil {
			host = hn
		}
	}
	res, err := FromDiag(FromDiagInput{
		StatsPath:   *stats,
		E2EPaths:    e2e,
		MetricsPath: *metrics,
		BrowserPath: *browser,
		Meta: MetaOverrides{
			Host: host, GPU: *metaGPU, Driver: *metaDriver, OS: *metaOS,
			Browser: *metaBrowser, Resolution: *metaRes,
			TargetFPS: uint32(*metaFPS), DurationSec: *metaDur,
		},
		Gates: gates,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "desktopreport from-diag: %v\n", err)
		return 2
	}
	jb, err := MarshalResult(&res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "desktopreport from-diag: %v\n", err)
		return 2
	}
	if *out == "" {
		fmt.Println(string(jb))
		return 0
	}
	if err := os.WriteFile(*out, append(jb, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "desktopreport from-diag: %v\n", err)
		return 2
	}
	fmt.Printf("from-diag: verdict %s -> %s\n", res.Verdict, *out)
	return 0
}

// repeat builds a slice of n copies (used for the separator row).
func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// multiFlag collects a repeatable string flag (-e2e a.json -e2e b.json).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
