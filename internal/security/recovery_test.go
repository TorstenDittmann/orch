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

func TestBackupSizesAndContentKeys(t *testing.T) {
	private, public, _ := NewRecoveryKey(rand.Reader)
	other, _, _ := NewRecoveryKey(rand.Reader)
	for _, size := range []int{0, 1, recoveryChunkSize - 1, recoveryChunkSize, recoveryChunkSize + 1, 3*recoveryChunkSize + 7} {
		plaintext := make([]byte, size)
		_, _ = rand.Read(plaintext)
		var encrypted bytes.Buffer
		if err := EncryptBackup(&encrypted, bytes.NewReader(plaintext), public, rand.Reader); err != nil {
			t.Fatal(err)
		}
		if got := EncryptedBackupSize(uint64(size)); got != uint64(encrypted.Len()) {
			t.Fatalf("size %d: EncryptedBackupSize = %d, want %d", size, got, encrypted.Len())
		}
		if plain, ok := PlainBackupSize(uint64(encrypted.Len())); !ok || plain != uint64(size) {
			t.Fatalf("size %d: PlainBackupSize = %d, %v", size, plain, ok)
		}
		key, err := BackupContentKey(encrypted.Bytes()[:EncryptedBackupHeaderSize], private)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := DecryptBackupWithContentKey(bufio.NewReader(bytes.NewReader(encrypted.Bytes())), key)
		if err != nil {
			t.Fatal(err)
		}
		decrypted, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("size %d: decrypt with content key = %d bytes, %v", size, len(decrypted), err)
		}
		if _, err := BackupContentKey(encrypted.Bytes()[:EncryptedBackupHeaderSize], other); !errors.Is(err, ErrRecoveryKey) {
			t.Fatalf("size %d: content key for another recovery key = %v", size, err)
		}
		wrong := bytes.Repeat([]byte{1}, 32)
		reader, err = DecryptBackupWithContentKey(bufio.NewReader(bytes.NewReader(encrypted.Bytes())), wrong)
		if err == nil {
			_, err = io.ReadAll(reader)
		}
		if err == nil {
			t.Fatalf("size %d: a wrong content key opened the backup", size)
		}
	}
	if _, ok := PlainBackupSize(10); ok {
		t.Fatal("PlainBackupSize accepted a size shorter than the header")
	}
	if _, ok := PlainBackupSize(EncryptedBackupSize(recoveryChunkSize) + 1); ok {
		t.Fatal("PlainBackupSize accepted a size between two valid ones")
	}
}
