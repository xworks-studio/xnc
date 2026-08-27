// schema.go - the M4 Task 1 RunResult schema, gates, and the pure logic
// behind the `verify` subcommand.
//
// The struct is EXACTLY the task brief's declaration (no json tags): the
// wire format is Go-default field names, locked by TestRunResultJSONFieldNamesExact.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
)

// RunResult is one validation run's evidence record. Counters and
// percentiles ONLY - never raw pixels (milestone global constraint; ruling 6).
type RunResult struct {
	Host, GPU, Driver, OS, Browser                   string
	Resolution                                       string
	TargetFPS                                        uint32
	DurationSec                                      uint64
	OldFrameRegressions, EpochRegressions            uint64
	UnrecoveredFreezes                               uint64
	CaptureToAUP95Ms, InputToPresentP95Ms            float64
	QueueP95Ms, QueueMaxMs, CPUPercent, WorkingSetMB float64
	Verdict                                          string
}

// Gates are the numeric pass thresholds (exclusive upper bounds). The P0
// counters (OldFrameRegressions, EpochRegressions, UnrecoveredFreezes) are
// NOT gate-configurable: they must be zero, always.
type Gates struct {
	CaptureToAUP95Ms    float64
	InputToPresentP95Ms float64
	QueueP95Ms          float64
	QueueMaxMs          float64
	CPUPercent          float64
	WorkingSetMB        float64
}

// DefaultGates: the plan controller's default gate profile.
func DefaultGates() Gates {
	return Gates{
		CaptureToAUP95Ms:    15,
		InputToPresentP95Ms: 150,
		QueueP95Ms:          50,
		QueueMaxMs:          100,
		CPUPercent:          15,
		WorkingSetMB:        350,
	}
}

// Violation is one failed rule: a nonzero P0 counter (P0=true, Limit=0) or a
// gate value over its exclusive limit.
type Violation struct {
	Field string
	Value float64
	Limit float64
	P0    bool
}

func (v Violation) String() string {
	if v.P0 {
		return fmt.Sprintf("%s=%s (P0 counter must be zero)", v.Field, formatCount(v.Value))
	}
	return fmt.Sprintf("%s=%s exceeds gate %s", v.Field, formatMs(v.Value), formatMs(v.Limit))
}

// CheckResult applies the P0 rules + gate profile to one RunResult. Empty
// result = pass. The stored Verdict field is NOT consulted: verify is
// authoritative and recomputes.
func CheckResult(r *RunResult, g Gates) []Violation {
	var out []Violation
	p0 := []struct {
		name  string
		value uint64
	}{
		{"OldFrameRegressions", r.OldFrameRegressions},
		{"EpochRegressions", r.EpochRegressions},
		{"UnrecoveredFreezes", r.UnrecoveredFreezes},
	}
	for _, c := range p0 {
		if c.value != 0 {
			out = append(out, Violation{Field: c.name, Value: float64(c.value), P0: true})
		}
	}
	gates := []struct {
		name  string
		value float64
		limit float64
	}{
		{"CaptureToAUP95Ms", r.CaptureToAUP95Ms, g.CaptureToAUP95Ms},
		{"InputToPresentP95Ms", r.InputToPresentP95Ms, g.InputToPresentP95Ms},
		{"QueueP95Ms", r.QueueP95Ms, g.QueueP95Ms},
		{"QueueMaxMs", r.QueueMaxMs, g.QueueMaxMs},
		{"CPUPercent", r.CPUPercent, g.CPUPercent},
		{"WorkingSetMB", r.WorkingSetMB, g.WorkingSetMB},
	}
	for _, c := range gates {
		if c.value > c.limit {
			out = append(out, Violation{Field: c.name, Value: c.value, Limit: c.limit})
		}
	}
	return out
}

// MarshalResult emits the exact wire form (indented; field order = struct).
func MarshalResult(r *RunResult) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// UnmarshalResult parses one RunResult JSON document.
func UnmarshalResult(jb []byte) (RunResult, error) {
	var r RunResult
	if err := json.Unmarshal(jb, &r); err != nil {
		return RunResult{}, err
	}
	return r, nil
}

// RunResultJSONFields lists the wire field names, in order (reflection over
// the struct so the schema stays the single source of truth).
func RunResultJSONFields() []string {
	t := reflect.TypeOf(RunResult{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i).Name)
	}
	return out
}

// LoadResult reads one RunResult JSON file.
func LoadResult(path string) (RunResult, error) {
	jb, err := os.ReadFile(path)
	if err != nil {
		return RunResult{}, err
	}
	return UnmarshalResult(jb)
}

// LoadGates reads a partial gates override JSON (keys = the six RunResult
// gate field names) on top of DefaultGates. Unknown keys are errors so a
// typo cannot silently relax a gate.
func LoadGates(path string) (Gates, error) {
	g := DefaultGates()
	jb, err := os.ReadFile(path)
	if err != nil {
		return g, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(jb, &raw); err != nil {
		return g, fmt.Errorf("gates %s: %w", path, err)
	}
	val := reflect.ValueOf(&g).Elem()
	for k, v := range raw {
		f := val.FieldByName(k)
		if !f.IsValid() || f.Kind() != reflect.Float64 {
			return DefaultGates(), fmt.Errorf("gates %s: unknown gate %q", path, k)
		}
		var n float64
		if err := json.Unmarshal(v, &n); err != nil {
			return DefaultGates(), fmt.Errorf("gates %s: %s: %w", path, k, err)
		}
		f.SetFloat(n)
	}
	return g, nil
}

// Percentile returns the nearest-rank percentile (p in [0,1]) of vals.
// Input order is irrelevant (sorted copy); empty input -> 0.
func Percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := make([]float64, len(vals))
	copy(s, vals)
	for i := 1; i < len(s); i++ { // insertion sort: inputs are small sample sets
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	rank := int(math.Ceil(p * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

// formatCount renders an integer-valued counter without a decimal tail.
func formatCount(v float64) string {
	return strconv.FormatUint(uint64(v), 10)
}

// formatMs renders a metric with one decimal place.
func formatMs(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}
