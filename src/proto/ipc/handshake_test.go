package ipc

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestProofRFC4231VectorV2(t *testing.T) {
	got := Proof([]byte("Jefe"), []byte("what do ya want for nothing?"))
	want := "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
	if hex.EncodeToString(got) != want {
		t.Fatalf("HMAC vector mismatch: %x", got)
	}
	if !VerifyProof([]byte("Jefe"), []byte("what do ya want for nothing?"), got) {
		t.Fatal("VerifyProof should accept correct proof")
	}
	bad := append([]byte(nil), got...)
	bad[0] ^= 1
	if VerifyProof([]byte("Jefe"), []byte("what do ya want for nothing?"), bad) {
		t.Fatal("VerifyProof must reject tampered proof")
	}
	if VerifyProof([]byte("wrong"), []byte("what do ya want for nothing?"), got) {
		t.Fatal("VerifyProof must reject wrong secret")
	}
}

func TestHelloPayloadRoundTrip(t *testing.T) {
	nonce := NewNonce()
	if len(nonce) != NonceSize {
		t.Fatalf("nonce size %d", len(nonce))
	}
	pid, back, err := DecodeHello(EncodeHello(1234, nonce))
	if err != nil || pid != 1234 || !bytes.Equal(back, nonce) {
		t.Fatalf("hello round-trip: %v %d", err, pid)
	}
	p2, n2, proof, err := DecodeHelloProof(EncodeHelloProof(5678, nonce, Proof(nonce, nonce)))
	if err != nil || p2 != 5678 || !bytes.Equal(n2, nonce) || len(proof) != ProofSize {
		t.Fatalf("hello_proof round-trip: %v", err)
	}
	prf, err := DecodeProof(EncodeProof(proof))
	if err != nil || !bytes.Equal(prf, proof) {
		t.Fatalf("proof round-trip: %v", err)
	}
	for _, tc := range []struct {
		name string
		f    func() error
	}{
		{"hello short", func() error { _, _, err := DecodeHello(make([]byte, 4)); return err }},
		{"hello_proof short", func() error { _, _, _, err := DecodeHelloProof(make([]byte, 20)); return err }},
		{"proof short", func() error { _, err := DecodeProof(make([]byte, 31)); return err }},
	} {
		if tc.f() == nil {
			t.Fatalf("%s: want error", tc.name)
		}
	}
}
