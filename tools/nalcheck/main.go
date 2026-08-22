// nalcheck - Annex-B H.264 elementary stream analyzer for XNC diag gates.
//
// Counts access units (VCL slice NALs), IDR frames and their indices, SPS/PPS
// presence, bytes and (with --duration) average bitrate, and enforces the
// M1-Slice1 acceptance gates on top:
//
//	nalcheck <file.h264> [--json] [--max-idr-ratio <0..1>] [--duration <sec>]
//
// Exit codes: 0 = analysis ok and all assertions pass; 1 = parse failure or
// assertion failure; 2 = usage error.
//
// Hard invariants (independent of flags), from spec 7.10 shaping contract:
//   - a stream with zero start codes is a parse failure, not "0 frames";
//   - every IDR must be preceded by SPS+PPS (within the SEI/AUD/SPS/PPS run
//     in front of it), else the gate fails.
//
// Requested-vs-spontaneous keyframes: stats.json "keyframes" counts what the
// pipeline *requested* (force-key accounting); nalcheck counts IDRs actually
// present in the bitstream - the software MFT may emit spontaneous scene-cut
// IDRs, so the storm definition is an IDR *ratio* gate (--max-idr-ratio),
// not an absolute count.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Report is the full analysis result; --json marshals it verbatim.
type Report struct {
	File           string  `json:"file"`
	Bytes          int64   `json:"bytes"`
	NalCount       int     `json:"nal_count"`
	Frames         int     `json:"frames"`           // VCL slice NALs (types 1-5) = AUs for single-slice output
	IdrFrames      int     `json:"idr_frames"`       // type 5
	NonIdrFrames   int     `json:"non_idr_frames"`   // types 1-4
	IdrRatio       float64 `json:"idr_ratio"`        // IdrFrames/Frames (0 when no frames)
	IdrIndices     []int   `json:"idr_indices"`      // frame indices of IDRs
	SpsCount       int     `json:"sps_count"`        // type 7
	PpsCount       int     `json:"pps_count"`        // type 8
	SpsPpsComplete bool    `json:"sps_pps_complete"` // every IDR preceded by SPS+PPS
	AudCount       int     `json:"aud_count"`        // type 9 (should be 0: pipeline drops AUDs)
	SeiCount       int     `json:"sei_count"`        // type 6
	DurationSec    float64 `json:"duration_s,omitempty"`
	BitrateMbps    float64 `json:"bitrate_mbps,omitempty"` // requires --duration
}

// H.264 NAL unit types (header & 0x1F), only the ones we care about.
const (
	nalSliceNonIDR = 1
	nalSliceIDR    = 5
	nalSEI         = 6
	nalSPS         = 7
	nalPPS         = 8
	nalAUD         = 9
)

func isVCL(t int) bool { return t >= 1 && t <= 5 }

// nalUnit is one parsed NAL: header byte at Body, ends at End (start of the
// next start code; trailing zero bytes stay attributed to this unit, which is
// irrelevant for counting).
type nalUnit struct {
	Typ  int
	Body int
	End  int
}

// findStartCodes returns the byte offset and length of every Annex-B start
// code (00 00 01 and 00 00 00 01) in data. Leading garbage before the first
// code is ignored; RBSP emulation prevention (00 00 03) guarantees no false
// positives inside payload.
func findStartCodes(data []byte) [][2]int {
	var codes [][2]int // {offset, length}
	for i := 0; i+3 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		if data[i+2] == 1 {
			codes = append(codes, [2]int{i, 3})
		} else if data[i+2] == 0 && data[i+3] == 1 {
			codes = append(codes, [2]int{i, 4})
			i++ // the 4-byte code consumes one extra zero
		}
	}
	return codes
}

// ParseStream scans an Annex-B byte stream and produces the Report.
func ParseStream(data []byte) (*Report, error) {
	r := &Report{
		Bytes:          int64(len(data)),
		IdrIndices:     []int{},
		SpsPpsComplete: true,
	}
	codes := findStartCodes(data)
	if len(codes) == 0 {
		return nil, fmt.Errorf("no Annex-B start code (00 00 01) found in %d bytes - not an Annex-B H.264 stream", len(data))
	}
	var nals []nalUnit
	for k, c := range codes {
		body := c[0] + c[1]
		end := len(data)
		if k+1 < len(codes) {
			end = codes[k+1][0]
		}
		if body >= end { // empty unit between adjacent start codes: skip
			continue
		}
		nals = append(nals, nalUnit{Typ: int(data[body] & 0x1F), Body: body, End: end})
	}
	if len(nals) == 0 {
		return nil, fmt.Errorf("start codes found but no NAL payload (stream of %d bytes is empty units only)", len(data))
	}

	frameIdx := -1
	for _, n := range nals {
		r.NalCount++
		switch {
		case isVCL(n.Typ):
			frameIdx++
			r.Frames++
			if n.Typ == nalSliceIDR {
				r.IdrFrames++
				r.IdrIndices = append(r.IdrIndices, frameIdx)
			} else {
				r.NonIdrFrames++
			}
		case n.Typ == nalSPS:
			r.SpsCount++
		case n.Typ == nalPPS:
			r.PpsCount++
		case n.Typ == nalAUD:
			r.AudCount++
		case n.Typ == nalSEI:
			r.SeiCount++
		}
	}
	if r.Frames > 0 {
		r.IdrRatio = float64(r.IdrFrames) / float64(r.Frames)
	}

	// Shaping contract: every IDR preceded (through the parameter-set/SEI/AUD
	// run) by at least one SPS and one PPS.
	for i, n := range nals {
		if n.Typ != nalSliceIDR {
			continue
		}
		var seenSPS, seenPPS bool
		for j := i - 1; j >= 0; j-- {
			t := nals[j].Typ
			if t != nalSEI && t != nalSPS && t != nalPPS && t != nalAUD {
				break
			}
			if t == nalSPS {
				seenSPS = true
			}
			if t == nalPPS {
				seenPPS = true
			}
		}
		if !seenSPS || !seenPPS {
			r.SpsPpsComplete = false
		}
	}
	return r, nil
}

// GateResult is the outcome of the assertion set.
type GateResult struct {
	Pass   bool
	Reason string
}

// EvaluateGate applies the always-on invariant plus the optional
// --max-idr-ratio assertion.
func EvaluateGate(r *Report, maxRatio float64, hasMax bool) GateResult {
	if r.IdrFrames > 0 && !r.SpsPpsComplete {
		return GateResult{Pass: false, Reason: "sps-pps: IDR present without preceding SPS+PPS (shaping contract, spec 7.10)"}
	}
	if hasMax && r.IdrRatio > maxRatio {
		return GateResult{Pass: false, Reason: fmt.Sprintf("idr-ratio: %.4f exceeds max %.4f (E2-class keyframe storm)", r.IdrRatio, maxRatio)}
	}
	return GateResult{Pass: true}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: nalcheck <file.h264> [--json] [--max-idr-ratio <0..1>] [--duration <sec>]\n")
}

func run(args []string) int {
	var (
		file        string
		asJSON      bool
		maxRatio    float64
		hasMaxRatio bool
		durationSec float64
	)
	rest := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--max-idr-ratio":
			if i+1 >= len(args) {
				usage()
				return 2
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%g", &maxRatio); err != nil || maxRatio < 0 || maxRatio > 1 {
				fmt.Fprintf(os.Stderr, "nalcheck: --max-idr-ratio must be a number in [0,1], got %q\n", args[i])
				return 2
			}
			hasMaxRatio = true
		case "--duration":
			if i+1 >= len(args) {
				usage()
				return 2
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%g", &durationSec); err != nil || durationSec <= 0 {
				fmt.Fprintf(os.Stderr, "nalcheck: --duration must be a positive number of seconds, got %q\n", args[i])
				return 2
			}
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) != 1 {
		usage()
		return 2
	}
	file = rest[0]

	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nalcheck: read %s: %v\n", file, err)
		return 1
	}
	r, err := ParseStream(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nalcheck: %s: %v\n", file, err)
		return 1
	}
	r.File = file
	if durationSec > 0 {
		r.DurationSec = durationSec
		r.BitrateMbps = float64(r.Bytes) * 8 / durationSec / 1e6
	}

	gate := EvaluateGate(r, maxRatio, hasMaxRatio)

	if asJSON {
		out, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Printf("file=%s\n", file)
		fmt.Printf("bytes=%d nal_units=%d\n", r.Bytes, r.NalCount)
		fmt.Printf("frames=%d (idr=%d non_idr=%d) idr_ratio=%.4f (%.2f%%)\n",
			r.Frames, r.IdrFrames, r.NonIdrFrames, r.IdrRatio, r.IdrRatio*100)
		fmt.Printf("idr_indices=%v\n", r.IdrIndices)
		fmt.Printf("sps=%d pps=%d sps_pps_complete=%v aud=%d sei=%d\n",
			r.SpsCount, r.PpsCount, r.SpsPpsComplete, r.AudCount, r.SeiCount)
		if durationSec > 0 {
			fmt.Printf("duration=%.1fs bitrate=%.3f Mbps\n", r.DurationSec, r.BitrateMbps)
		}
	}
	if hasMaxRatio {
		fmt.Printf("assert max_idr_ratio<=%.4f: %s\n", maxRatio, passFail(gate.Pass && r.IdrRatio <= maxRatio))
	}
	if !gate.Pass {
		fmt.Printf("FAIL: %s\n", gate.Reason)
		return 1
	}
	fmt.Println("nalcheck: OK")
	return 0
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func main() {
	os.Exit(run(os.Args[1:]))
}
