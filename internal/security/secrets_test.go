package security

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecretBoxRoundTripAndAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{7}, SecretKeySize)
	box, err := NewSecretBox(key, bytes.NewReader(bytes.Repeat([]byte{9}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal([]byte("cluster\x00app\x00name\x00version"), []byte("sensitive-value"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := box.Open([]byte("cluster\x00app\x00name\x00version"), encrypted)
	if err != nil || string(plain) != "sensitive-value" {
		t.Fatalf("open = %q, %v", plain, err)
	}
	if bytes.Contains(encrypted.Ciphertext, []byte("sensitive-value")) || bytes.Contains(encrypted.WrappedKey, []byte("sensitive-value")) {
		t.Fatal("ciphertext contains plaintext")
	}
	encrypted.Ciphertext[0] ^= 1
	if _, err := box.Open([]byte("cluster\x00app\x00name\x00version"), encrypted); !errors.Is(err, ErrSecretAuthentication) {
		t.Fatalf("tampered open = %v", err)
	}
}

func TestLoadOrCreateSecretBoxProtectsKey(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "secret.key")
	box, created, err := LoadOrCreateSecretBox(path, bytes.NewReader(bytes.Repeat([]byte{3}, 64)))
	if err != nil || !created || box.Fingerprint() == "" {
		t.Fatalf("create = %v, %v", created, err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode().Perm(), err)
	}
	loaded, created, err := LoadOrCreateSecretBox(path, nil)
	if err != nil || created || loaded.Fingerprint() != box.Fingerprint() {
		t.Fatalf("reload = %v, %v", created, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateSecretBox(path, nil); err == nil {
		t.Fatal("unsafe key permissions accepted")
	}
}

func TestSecretRequestHMACIsKeyed(t *testing.T) {
	first, _ := NewSecretBox(bytes.Repeat([]byte{1}, SecretKeySize), nil)
	second, _ := NewSecretBox(bytes.Repeat([]byte{2}, SecretKeySize), nil)
	if first.RequestHMAC([]byte("low-entropy")) == second.RequestHMAC([]byte("low-entropy")) {
		t.Fatal("request fingerprint is not keyed")
	}
}

func TestSecretBoxRejectsInvalidNonceLengths(t *testing.T) {
	box, err := NewSecretBox(bytes.Repeat([]byte{4}, SecretKeySize), nil)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Seal([]byte("aad"), []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	for _, wrapping := range []bool{false, true} {
		for _, size := range []int{0, 1, 11, 13, 64} {
			broken := encrypted
			if wrapping {
				broken.WrappedKeyNonce = make([]byte, size)
			} else {
				broken.Nonce = make([]byte, size)
			}
			if _, err := box.Open([]byte("aad"), broken); !errors.Is(err, ErrSecretAuthentication) {
				t.Fatalf("invalid nonce accepted: %v", err)
			}
		}
	}
}
