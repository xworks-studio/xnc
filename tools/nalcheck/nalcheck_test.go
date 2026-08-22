package main

import (
	"strings"
	"testing"
)

// startCode4/startCode3 append 4- and 3-byte Annex-B start codes. nalcheck
// must accept both (the pipeline normalizes to 4-byte, but the analyzer is a
// general Annex-B tool and real MF output may contain 3-byte codes).
func startCode4(b []byte) []byte { return append(b, 0x00, 0x00, 0x00, 0x01) }
func startCode3(b []byte) []byte { return append(b, 0x00, 0x00, 0x01) }

// nal appends one NAL unit behind a start code of the requested size. The
// payloads are synthetic bytes (real RBSP content is irrelevant for counting;
// emulation prevention keeps 00 00 01 from appearing inside, and the payloads
// below deliberately exercise trailing zeros).
func nal(b []byte, sc int, header byte, payload ...byte) []byte {
	if sc == 4 {
		b = startCode4(b)
	} else {
		b = startCode3(b)
	}
	b = append(b, header)
	return append(b, payload...)
}

// handBuiltStream is the plan's synthetic Annex-B stream: SPS/PPS + 3 IDR,
// 7 non-IDR slices, one SEI and one AUD (which must NOT count as frames),
// mixed 4/3-byte start codes, and a trailing 00 00 (cabac_zero_words tail —
// must not produce a phantom NAL).
//
//	expected AU sequence:  IDR#0 P P | IDR#1 P P P | IDR#2 P (AUD) P
func handBuiltStream() []byte {
	var b []byte
	b = nal(b, 4, 0x67, 0x4d, 0x40, 0x33, 0x96, 0x56, 0x01) // SPS
	b = nal(b, 4, 0x68, 0xeb, 0xec, 0xb2, 0x2c)             // PPS
	b = nal(b, 4, 0x06, 0x01, 0x02, 0x80)                   // SEI
	b = nal(b, 4, 0x65, 0x88, 0x84, 0x00, 0x10)             // IDR (frame 0)
	b = nal(b, 3, 0x41, 0x1a, 0x02, 0x00, 0x00)             // P    (frame 1; payload has trailing zeros)
	b = nal(b, 3, 0x41, 0x02, 0xd1)                         // P    (frame 2)
	b = nal(b, 4, 0x67, 0x4d, 0x40, 0x33, 0x96, 0x56, 0x01) // SPS (repeat before IDR)
	b = nal(b, 4, 0x68, 0xeb, 0xec, 0xb2, 0x2c)             // PPS
	b = nal(b, 4, 0x65, 0x88, 0x84, 0x2c)                   // IDR  (frame 3)
	b = nal(b, 3, 0x41, 0x11, 0x00, 0x03, 0x01)             // P    (frame 4; emulation prevention bytes)
	b = nal(b, 3, 0x41, 0x0f)                               // P    (frame 5)
	b = nal(b, 3, 0x41, 0x2e)                               // P    (frame 6)
	b = nal(b, 4, 0x67, 0x4d, 0x40, 0x33, 0x96, 0x56, 0x01) // SPS
	b = nal(b, 4, 0x68, 0xeb, 0xec, 0xb2, 0x2c)             // PPS
	b = nal(b, 4, 0x65, 0x88, 0x84, 0x0a)                   // IDR  (frame 7)
	b = nal(b, 3, 0x41, 0x9b)                               // P    (frame 8)
	b = nal(b, 3, 0x09, 0x10)                               // AUD  (not a frame)
	b = nal(b, 4, 0x41, 0x31)                               // P    (frame 9)
	return append(b, 0x00, 0x00)                            // cabac zero-word tail
}

func TestParseStreamCounts(t *testing.T) {
	data := handBuiltStream()
	r, err := ParseStream(data)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if r.Bytes != int64(len(data)) {
		t.Errorf("Bytes = %d, want %d", r.Bytes, len(data))
	}
	if r.NalCount != 18 {
		t.Errorf("NalCount = %d, want 18 (3 SPS + 3 PPS + 1 SEI + 1 AUD + 3 IDR + 7 P)", r.NalCount)
	}
	if r.Frames != 10 {
		t.Errorf("Frames = %d, want 10", r.Frames)
	}
	if r.IdrFrames != 3 {
		t.Errorf("IdrFrames = %d, want 3", r.IdrFrames)
	}
	if r.NonIdrFrames != 7 {
		t.Errorf("NonIdrFrames = %d, want 7", r.NonIdrFrames)
	}
	want := []int{0, 3, 7}
	if len(r.IdrIndices) != len(want) {
		t.Fatalf("IdrIndices = %v, want %v", r.IdrIndices, want)
	}
	for i, idx := range want {
		if r.IdrIndices[i] != idx {
			t.Errorf("IdrIndices[%d] = %d, want %d", i, r.IdrIndices[i], idx)
		}
	}
	if got := r.SpsCount; got != 3 {
		t.Errorf("SpsCount = %d, want 3", got)
	}
	if got := r.PpsCount; got != 3 {
		t.Errorf("PpsCount = %d, want 3", got)
	}
	if r.SpsPpsComplete != true {
		t.Errorf("SpsPpsComplete = false, want true (every IDR preceded by SPS+PPS)")
	}
	if r.AudCount != 1 {
		t.Errorf("AudCount = %d, want 1", r.AudCount)
	}
	if r.SeiCount != 1 {
		t.Errorf("SeiCount = %d, want 1", r.SeiCount)
	}
	if wantRatio := 3.0 / 10.0; r.IdrRatio < wantRatio-1e-9 || r.IdrRatio > wantRatio+1e-9 {
		t.Errorf("IdrRatio = %v, want %v", r.IdrRatio, wantRatio)
	}
}

func TestParseStreamIdrWithoutSpsPps(t *testing.T) {
	// Stream starts cold with an IDR and a second IDR appears mid-stream
	// with SPS but no PPS: the shaping contract ("IDR must be preceded by
	// SPS/PPS", spec 7.10) is violated in both places.
	var b []byte
	b = nal(b, 4, 0x65, 0x88, 0x84)       // IDR, cold — no SPS/PPS before
	b = nal(b, 3, 0x41, 0x11)             // P
	b = nal(b, 4, 0x67, 0x4d, 0x40, 0x33) // SPS only
	b = nal(b, 4, 0x65, 0x88, 0x85)       // IDR — PPS missing
	b = nal(b, 3, 0x41, 0x22)             // P
	r, err := ParseStream(b)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if r.SpsPpsComplete {
		t.Errorf("SpsPpsComplete = true, want false (2 IDRs lack SPS+PPS prefix)")
	}
	if r.Frames != 4 || r.IdrFrames != 2 {
		t.Errorf("Frames = %d IdrFrames = %d, want 4/2", r.Frames, r.IdrFrames)
	}
}

func TestParseStreamEmptyAndGarbage(t *testing.T) {
	if _, err := ParseStream(nil); err == nil {
		t.Errorf("ParseStream(nil) succeeded, want error")
	}
	if _, err := ParseStream([]byte{0x00, 0x00}); err == nil {
		t.Errorf("ParseStream(zeros) succeeded, want error (no start code)")
	}
	if _, err := ParseStream([]byte{0x12, 0x34, 0x56, 0x78, 0x9a}); err == nil {
		t.Errorf("ParseStream(garbage) succeeded, want error (no start code)")
	}
}

func TestParseStreamOnlyParameterSets(t *testing.T) {
	// No VCL NALs at all: not an error for parsing, but Frames==0 and the
	// ratio is defined as 0 (no IDR storm signal from an empty GOP).
	var b []byte
	b = nal(b, 4, 0x67, 0x4d, 0x40)
	b = nal(b, 4, 0x68, 0xeb, 0xec)
	r, err := ParseStream(b)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if r.Frames != 0 || r.IdrFrames != 0 || r.IdrRatio != 0 {
		t.Errorf("want 0/0/0, got %d/%d/%v", r.Frames, r.IdrFrames, r.IdrRatio)
	}
}

func TestEvaluateGate(t *testing.T) {
	mk := func(frames, idr int) *Report {
		return &Report{Frames: frames, IdrFrames: idr, IdrRatio: float64(idr) / float64(frames), SpsPpsComplete: true}
	}
	cases := []struct {
		name       string
		rep        *Report
		maxRatio   float64
		hasMax     bool
		wantPass   bool
		wantReason string
	}{
		{"ratio ok", mk(52, 2), 0.30, true, true, ""},
		{"storm", mk(20, 10), 0.30, true, false, "idr-ratio"},
		{"no assertion", mk(20, 10), 0, false, true, ""},
		{"shaping broken", &Report{Frames: 10, IdrFrames: 1, IdrRatio: 0.1, SpsPpsComplete: false}, 0.30, true, false, "sps-pps"},
	}
	for _, tc := range cases {
		got := EvaluateGate(tc.rep, tc.maxRatio, tc.hasMax)
		if got.Pass != tc.wantPass {
			t.Errorf("%s: Pass = %v, want %v (reason %q)", tc.name, got.Pass, tc.wantPass, got.Reason)
		}
		if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
			t.Errorf("%s: Reason = %q, want substring %q", tc.name, got.Reason, tc.wantReason)
		}
	}
}
