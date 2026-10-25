package security

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestBackupEncryptionRoundTripsAtChunkBoundaries(t *testing.T) {
	private, public, err := NewRecoveryKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidRecoveryPublicKey(public) || ValidRecoveryPublicKey(private) {
		t.Fatalf("ValidRecoveryPublicKey mixes up %q and %q", public, private)
	}
	for _, size := range []int{0, 1, recoveryChunkSize - 1, recoveryChunkSize, recoveryChunkSize + 1, 3 * recoveryChunkSize} {
		plaintext := make([]byte, size)
		_, _ = rand.Read(plaintext)
		var encrypted bytes.Buffer
		if err := EncryptBackup(&encrypted, bytes.NewReader(plaintext), public, rand.Reader); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encrypted.Bytes(), plaintext) && size > 16 {
			t.Fatalf("size %d: ciphertext contains the plaintext", size)
		}
		source := bufio.NewReader(bytes.NewReader(encrypted.Bytes()))
		if !IsEncryptedBackup(source) {
			t.Fatalf("size %d: IsEncryptedBackup = false", size)
		}
		reader, err := DecryptBackup(source, private)
		if err != nil {
			t.Fatal(err)
		}
		decrypted, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("size %d: decrypt = %d bytes, %v", size, len(decrypted), err)
		}
	}
}

func TestBackupDecryptionRefusesWrongKeysAndTampering(t *testing.T) {
	private, public, _ := NewRecoveryKey(rand.Reader)
	other, _, _ := NewRecoveryKey(rand.Reader)
	plaintext := bytes.Repeat([]byte("orch"), recoveryChunkSize)
	var encrypted bytes.Buffer
	if err := EncryptBackup(&encrypted, bytes.NewReader(plaintext), public, rand.Reader); err != nil {
		t.Fatal(err)
	}
	read := func(data []byte, key string) error {
		reader, err := DecryptBackup(bufio.NewReader(bytes.NewReader(data)), key)
		if err != nil {
			return err
		}
		_, err = io.ReadAll(reader)
		return err
	}
	if err := read(encrypted.Bytes(), other); !errors.Is(err, ErrRecoveryKey) {
		t.Fatalf("wrong key = %v, want ErrRecoveryKey", err)
	}
	if err := read(encrypted.Bytes(), public); err == nil || !strings.Contains(err.Error(), "public key") {
		t.Fatalf("public key = %v", err)
	}
	// Dropping the last chunk must not look like a shorter, valid backup.
	truncated := encrypted.Bytes()[:len(encrypted.Bytes())-1000]
	if err := read(truncated, private); err == nil {
		t.Fatal("truncated backup decrypted")
	}
	cut := len(encryptedBackupMagic) + 64 + recoveryChunkSize + 16
	if err := read(encrypted.Bytes()[:cut], private); err == nil {
		t.Fatal("backup cut at a chunk boundary decrypted")
	}
	flipped := append([]byte(nil), encrypted.Bytes()...)
	flipped[len(flipped)/2] ^= 1
	if err := read(flipped, private); err == nil {
		t.Fatal("tampered backup decrypted")
	}
}
