package cryptox

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, KeyLen)
	plain := `{"type":"vless","tag":"secret-node"}`
	sealed, err := Encrypt(key, plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if sealed == plain {
		t.Fatal("ciphertext equals plaintext")
	}
	got, err := Decrypt(key, sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("roundtrip mismatch: %q", got)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	key := bytes.Repeat([]byte{1}, KeyLen)
	other := bytes.Repeat([]byte{2}, KeyLen)
	sealed, _ := Encrypt(key, "hello")
	if _, err := Decrypt(other, sealed); err == nil {
		t.Fatal("decrypt with wrong key must fail")
	}
}

func TestEmptyPlaintext(t *testing.T) {
	key := bytes.Repeat([]byte{3}, KeyLen)
	sealed, err := Encrypt(key, "")
	if err != nil || sealed != "" {
		t.Fatalf("empty encrypt = %q, %v", sealed, err)
	}
	got, err := Decrypt(key, "")
	if err != nil || got != "" {
		t.Fatalf("empty decrypt = %q, %v", got, err)
	}
}
