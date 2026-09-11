// Package cryptox provides AES-256-GCM encryption helpers used to store node
// secrets and outbound configs at rest. Keys are 32 random bytes persisted to
// the data directory with 0600 permissions.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// KeyLen is the AES-256 key length in bytes.
const KeyLen = 32

// LoadOrCreateKey reads a KeyLen-byte key from <dataDir>/<name>, generating
// and persisting one when missing.
func LoadOrCreateKey(dataDir, name string) ([]byte, error) {
	path := filepath.Join(dataDir, name)
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) != KeyLen {
			return nil, fmt.Errorf("key file %s has %d bytes, want %d", path, len(raw), KeyLen)
		}
		return raw, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Encrypt seals plaintext into nonce||ciphertext, base64-encoded.
func Encrypt(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a value produced by Encrypt. Empty input yields empty output.
func Decrypt(key []byte, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
