package kce

import (
	"bytes"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// Vector generated with the reference Python implementation:
// kce.encrypt(plaintext, key).blob base64.
const (
	pythonKey      = "kce-interop-test-key"
	pythonPlainB64 = "5r6qIHNpbmctYm94IGh1YiBLQ0UxIGludGVyb3AgdmVjdG9yCmxpbmUyOiAwMTIzNDU2Nzg5Cg=="
	pythonBlobB64  = "S0NFMQBgTH4jwLbI457yOKqN8ER38ryPmIx+ezgAAAA3cJRwsxNLWlzamkPJl94GdyYFs+jZKmcSc20zEHmMJYsyCAFHY7+noYXLb7heSNL2AdhMMpqwHAp1Nx7hKr6ZPSM1YB9HOrc="
)

func TestDecryptPythonVector(t *testing.T) {
	blob, err := base64.StdEncoding.DecodeString(pythonBlobB64)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt(blob, pythonKey)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	want, _ := base64.StdEncoding.DecodeString(pythonPlainB64)
	if !bytes.Equal(pt, want) {
		t.Fatalf("plaintext mismatch:\n got %q\nwant %q", pt, want)
	}
}

func TestRoundTripAndFormat(t *testing.T) {
	key := "another-key-澪"
	pt := []byte("round trip 澪 payload \x00\x01\xff")
	blob, err := Encrypt(pt, key)
	if err != nil {
		t.Fatal(err)
	}
	if !IsBlob(blob) {
		t.Fatal("missing magic")
	}
	if got, err := Decrypt(blob, key); err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("round trip failed: %v %q", err, got)
	}
	// Same plaintext encrypts differently (fresh salt/iv).
	blob2, _ := Encrypt(pt, key)
	if bytes.Equal(blob, blob2) {
		t.Fatal("expected semantically secure output")
	}
	// Envelope framing: 4+1+16+8+4 header, 16-byte tag.
	if len(blob) != headerLen+len(pt)+tagBytes {
		t.Fatalf("unexpected envelope length %d", len(blob))
	}
}

func TestDecryptTamperAndWrongKey(t *testing.T) {
	blob, _ := Encrypt([]byte("secret payload"), "key-a")
	// Flip each of: header byte, ciphertext byte, tag byte; plus truncation.
	for name, mutate := range map[string]func([]byte) []byte{
		"header":    func(b []byte) []byte { b[1] ^= 1; return b },
		"cctext":    func(b []byte) []byte { b[headerLen] ^= 1; return b },
		"tag":       func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"truncated": func(b []byte) []byte { return b[:len(b)-1] },
		"extended":  func(b []byte) []byte { return append(b, 0) },
	} {
		if _, err := Decrypt(mutate(append([]byte{}, blob...)), "key-a"); err == nil {
			t.Errorf("%s: expected failure", name)
		}
	}
	if _, err := Decrypt(blob, "key-b"); err == nil {
		t.Error("wrong key: expected failure")
	}
	// All failures must be the single indistinguishable error.
	if _, err := Decrypt(blob[:10], "key-a"); !bytes.Equal([]byte(err.Error()), []byte(ErrInvalid.Error())) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestLargePlaintextMultiBlock(t *testing.T) {
	// 3.5 blocks + non-aligned tail exercises block-wise keystream expansion.
	pt := make([]byte, 200)
	for i := range pt {
		pt[i] = byte(i * 7)
	}
	blob, err := Encrypt(pt, "k")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(blob, "k")
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("multi-block round trip failed: %v", err)
	}
	if _, err := Encrypt(make([]byte, MaxPlaintextLen+1), "k"); err == nil {
		t.Error("expected size limit enforcement")
	}
}

func TestShakeSqueezeMatchesReference(t *testing.T) {
	// hashlib.shake_256(b"ab").digest(5) == Go SHAKE-256 squeeze.
	h := sha3.NewSHAKE256()
	h.Write([]byte("ab"))
	sum := make([]byte, 5)
	h.Read(sum)
	want, _ := hex.DecodeString("effb6ac214")
	if !bytes.Equal(sum, want) {
		t.Fatalf("SHAKE-256 squeeze mismatch: %x", sum)
	}
}
