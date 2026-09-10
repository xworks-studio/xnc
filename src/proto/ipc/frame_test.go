package ipc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"
)

// Shared wire vector V1 (also asserted by the C++ selftest, native/core):
// flags byte is 0x01 = FlagResponse per spec §9.2 bit0=response. The task
// brief's original hex said 0x05 there; corrected to 0x01 by owner decision
// 2026-08-22 so both languages assert the spec-conformant byte.
func TestFrameWireVectorV1(t *testing.T) {
	f := &Frame{Flags: FlagResponse, MessageType: MsgPing, RequestID: 0xDEADBEEF, Payload: []byte("xnc")}
	want, _ := hex.DecodeString("584E495001011000EFBEADDE03000000786E63")
	if got := f.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("wire mismatch:\n got  %x\n want %x", got, want)
	}
	back, err := Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	if back.Flags != FlagResponse || back.MessageType != MsgPing || back.RequestID != 0xDEADBEEF || string(back.Payload) != "xnc" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestDecodeHeaderErrors(t *testing.T) {
	good := (&Frame{MessageType: MsgPing}).Encode()
	if _, _, err := DecodeHeader(good[:15]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short header: %v", err)
	}
	bad := append([]byte(nil), good...)
	bad[0] = 'Y' // brief said 'X', but Magic "XNIP" already starts with 'X' — no-op
	if _, _, err := DecodeHeader(bad); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("magic: %v", err)
	}
	bad = append([]byte(nil), good...)
	bad[4] = 2
	if _, _, err := DecodeHeader(bad); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("version: %v", err)
	}
	bad = append([]byte(nil), good...)
	bad[12], bad[13], bad[14], bad[15] = 0xFF, 0xFF, 0xFF, 0x7F // >9MiB
	if _, _, err := DecodeHeader(bad); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if _, err := Decode(good[:len(good)-1]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("payload truncated: %v", err)
	}
}

func TestFrameStream(t *testing.T) {
	c1, c2 := net.Pipe()
	go func() {
		_ = WriteFrame(c1, &Frame{MessageType: MsgPong, RequestID: 7, Payload: []byte{1, 2}})
		_ = c1.Close()
	}()
	f, err := ReadFrame(c2)
	if err != nil {
		t.Fatal(err)
	}
	if f.MessageType != MsgPong || f.RequestID != 7 || !bytes.Equal(f.Payload, []byte{1, 2}) {
		t.Fatalf("stream frame mismatch: %+v", f)
	}
	if _, err := ReadFrame(c2); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestMaxFrameEnforced(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Encode should panic on oversize payload (programmer error)")
		}
	}()
	_ = (&Frame{Payload: make([]byte, MaxFrameBytes+1)}).Encode()
}
