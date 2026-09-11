// Package kce implements the KCE1 envelope (KataBump Container Envelope v1)
// in Go, byte-compatible with the reference Python implementation:
// encrypted distribution of Clash client configs over plain HTTP.
// Format (all multi-byte integers big-endian):
//
//	"KCE1" | flags(1) | salt(16) | iv(8) | ct_len(4) | ct | tag(16)
//
//	key = SHAKE-256(key_material || salt, 32 bytes)
//	keystream = SHAKE-256(key || iv) expanded in 64-byte blocks
//	ct = pt XOR keystream
//	tag = SHAKE-256(key || iv || ct_len || ct, 16 bytes)  (encrypt-then-MAC)
//
// Failures are reported as a single error type without distinguishing wrong
// key from corrupted data, mirroring the reference behaviour.
package kce

import (
	"crypto/rand"
	"crypto/sha3"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

const (
	magic      = "KCE1"
	versionFlg = 0
	saltBytes  = 16
	ivBytes    = 8
	tagBytes   = 16
	keyBytes   = 32
	blockBytes = 64
	headerLen  = len(magic) + 1 + saltBytes + ivBytes + 4
	// MaxPlaintextLen matches the reference implementation limit.
	MaxPlaintextLen = 4 * 1024 * 1024
)

// ErrInvalid covers every failure mode (bad framing, truncated or tampered
// ciphertext, wrong key) with one indistinguishable error.
var ErrInvalid = errors.New("kce: invalid ciphertext or key")

// IsBlob reports whether data carries the KCE1 magic (cipher vs plain form).
func IsBlob(data []byte) bool {
	return len(data) >= len(magic) && string(data[:len(magic)]) == magic
}

// Encrypt seals plaintext into a KCE1 envelope with a fresh random salt/iv.
func Encrypt(plaintext []byte, keyMaterial string) ([]byte, error) {
	if len(keyMaterial) == 0 {
		return nil, errors.New("kce: empty key material")
	}
	if len(plaintext) > MaxPlaintextLen {
		return nil, errors.New("kce: plaintext exceeds 4 MiB limit")
	}
	salt := randomBytes(saltBytes)
	iv := randomBytes(ivBytes)
	key := deriveKey(keyMaterial, salt)
	ct := xorStream(key, iv, plaintext)
	out := make([]byte, 0, headerLen+len(ct)+tagBytes)
	out = append(out, magic...)
	out = append(out, versionFlg)
	out = append(out, salt...)
	out = append(out, iv...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(ct)))
	out = append(out, ct...)
	out = append(out, tag(key, iv, ct)...)
	return out, nil
}

// Decrypt opens a KCE1 envelope produced by Encrypt or kce.py.
func Decrypt(blob []byte, keyMaterial string) ([]byte, error) {
	if len(blob) < headerLen+tagBytes || !IsBlob(blob) {
		return nil, ErrInvalid
	}
	if blob[len(magic)] != versionFlg {
		return nil, ErrInvalid
	}
	off := len(magic) + 1
	salt := blob[off : off+saltBytes]
	off += saltBytes
	iv := blob[off : off+ivBytes]
	off += ivBytes
	ctLen := binary.BigEndian.Uint32(blob[off:])
	off += 4
	if uint64(len(blob)) != uint64(off)+uint64(ctLen)+tagBytes {
		return nil, ErrInvalid
	}
	ct := blob[off : off+int(ctLen)]
	key := deriveKey(keyMaterial, salt)
	want := tag(key, iv, ct)
	if subtle.ConstantTimeCompare(want, blob[off+int(ctLen):]) != 1 {
		return nil, ErrInvalid
	}
	return xorStream(key, iv, ct), nil
}

// deriveKey = SHAKE-256(key_material || salt) squeezed to 32 bytes.
func deriveKey(keyMaterial string, salt []byte) []byte {
	h := sha3.NewSHAKE256()
	h.Write([]byte(keyMaterial))
	h.Write(salt)
	out := make([]byte, keyBytes)
	h.Read(out)
	return out
}

// tag = SHAKE-256(key || iv || ct_len(4, BE) || ct) squeezed to 16 bytes.
func tag(key, iv, ct []byte) []byte {
	h := sha3.NewSHAKE256()
	h.Write(key)
	h.Write(iv)
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(ct))))
	h.Write(ct)
	out := make([]byte, tagBytes)
	h.Read(out)
	return out
}

// xorStream expands SHAKE-256(key || iv) in 64-byte blocks and XORs it over
// data, returning the result (input left untouched).
func xorStream(key, iv, data []byte) []byte {
	h := sha3.NewSHAKE256()
	h.Write(key)
	h.Write(iv)
	out := make([]byte, len(data))
	for start := 0; start < len(data); start += blockBytes {
		block := make([]byte, min(blockBytes, len(data)-start))
		h.Read(block)
		for i := range block {
			out[start+i] = data[start+i] ^ block[i]
		}
	}
	return out
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read is documented to never fail on modern platforms; a
	// failure here means the process is fundamentally broken.
	if _, err := rand.Read(b); err != nil {
		panic("kce: crypto/rand unavailable: " + err.Error())
	}
	return b
}
