package security

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const SecretKeySize = 32

var ErrSecretAuthentication = errors.New("secret ciphertext authentication failed")

// EncryptedValue contains an envelope-encrypted value. The data key is wrapped
// independently so a future release can rotate the key-encryption key without
// rewriting value ciphertext.
type EncryptedValue struct {
	Algorithm       string
	Nonce           []byte
	Ciphertext      []byte
	WrappedKeyNonce []byte
	WrappedKey      []byte
}

// SecretBox owns a process-local copy of a 256-bit key-encryption key.
type SecretBox struct {
	key         [SecretKeySize]byte
	fingerprint string
	random      io.Reader
}

func NewSecretBox(key []byte, randomSource io.Reader) (*SecretBox, error) {
	if len(key) != SecretKeySize {
		return nil, fmt.Errorf("secret key must contain exactly %d bytes", SecretKeySize)
	}
	if randomSource == nil {
		randomSource = rand.Reader
	}
	box := &SecretBox{random: randomSource}
	copy(box.key[:], key)
	sum := sha256.Sum256(key)
	box.fingerprint = hex.EncodeToString(sum[:])
	return box, nil
}

func (b *SecretBox) Fingerprint() string {
	if b == nil {
		return ""
	}
	return b.fingerprint
}

func (b *SecretBox) Seal(aad, plaintext []byte) (EncryptedValue, error) {
	if b == nil {
		return EncryptedValue{}, errors.New("secret box is unavailable")
	}
	dataKey := make([]byte, SecretKeySize)
	if _, err := io.ReadFull(b.random, dataKey); err != nil {
		return EncryptedValue{}, fmt.Errorf("generate secret data key: %w", err)
	}
	defer wipe(dataKey)
	dataAEAD, err := newGCM(dataKey)
	if err != nil {
		return EncryptedValue{}, err
	}
	nonce := make([]byte, dataAEAD.NonceSize())
	if _, err := io.ReadFull(b.random, nonce); err != nil {
		return EncryptedValue{}, fmt.Errorf("generate secret nonce: %w", err)
	}
	ciphertext := dataAEAD.Seal(nil, nonce, plaintext, aad)

	wrappingAEAD, err := newGCM(b.key[:])
	if err != nil {
		return EncryptedValue{}, err
	}
	wrappedKeyNonce := make([]byte, wrappingAEAD.NonceSize())
	if _, err := io.ReadFull(b.random, wrappedKeyNonce); err != nil {
		return EncryptedValue{}, fmt.Errorf("generate wrapped-key nonce: %w", err)
	}
	wrappingAAD := append(append([]byte(nil), aad...), []byte("\x00data-key")...)
	wrappedKey := wrappingAEAD.Seal(nil, wrappedKeyNonce, dataKey, wrappingAAD)
	return EncryptedValue{
		Algorithm: "aes-256-gcm-envelope-v1", Nonce: nonce, Ciphertext: ciphertext,
		WrappedKeyNonce: wrappedKeyNonce, WrappedKey: wrappedKey,
	}, nil
}

func (b *SecretBox) Open(aad []byte, encrypted EncryptedValue) ([]byte, error) {
	if b == nil {
		return nil, errors.New("secret box is unavailable")
	}
	if encrypted.Algorithm != "aes-256-gcm-envelope-v1" {
		return nil, fmt.Errorf("unsupported secret encryption algorithm")
	}
	wrappingAEAD, err := newGCM(b.key[:])
	if err != nil {
		return nil, err
	}
	wrappingAAD := append(append([]byte(nil), aad...), []byte("\x00data-key")...)
	dataKey, err := wrappingAEAD.Open(nil, encrypted.WrappedKeyNonce, encrypted.WrappedKey, wrappingAAD)
	if err != nil {
		return nil, ErrSecretAuthentication
	}
	defer wipe(dataKey)
	dataAEAD, err := newGCM(dataKey)
	if err != nil {
		return nil, ErrSecretAuthentication
	}
	plaintext, err := dataAEAD.Open(nil, encrypted.Nonce, encrypted.Ciphertext, aad)
	if err != nil {
		return nil, ErrSecretAuthentication
	}
	return plaintext, nil
}

func (b *SecretBox) RequestHMAC(parts ...[]byte) string {
	mac := hmac.New(sha256.New, b.key[:])
	for _, part := range parts {
		var length [8]byte
		value := uint64(len(part))
		for index := len(length) - 1; index >= 0; index-- {
			length[index] = byte(value)
			value >>= 8
		}
		_, _ = mac.Write(length[:])
		_, _ = mac.Write(part)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// SecretAssociatedData encodes identity fields without delimiter ambiguity.
func SecretAssociatedData(parts ...string) []byte {
	var result bytes.Buffer
	for _, part := range parts {
		_ = binary.Write(&result, binary.BigEndian, uint32(len(part)))
		_, _ = result.WriteString(part)
	}
	return result.Bytes()
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize secret AEAD: %w", err)
	}
	return aead, nil
}

// LoadOrCreateSecretBox loads a protected key file or atomically creates one.
// It never follows symlinks and never repairs unsafe permissions silently.
func LoadOrCreateSecretBox(path string, randomSource io.Reader) (*SecretBox, bool, error) {
	if randomSource == nil {
		randomSource = rand.Reader
	}
	key, err := readSecretKey(path)
	if err == nil {
		box, boxErr := NewSecretBox(key, randomSource)
		wipe(key)
		return box, false, boxErr
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	key = make([]byte, SecretKeySize)
	if _, err := io.ReadFull(randomSource, key); err != nil {
		return nil, false, fmt.Errorf("generate secret key: %w", err)
	}
	defer wipe(key)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			loaded, loadErr := readSecretKey(path)
			if loadErr != nil {
				return nil, false, loadErr
			}
			defer wipe(loaded)
			box, boxErr := NewSecretBox(loaded, randomSource)
			return box, false, boxErr
		}
		return nil, false, fmt.Errorf("create secret key: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(key); err != nil {
		return nil, false, fmt.Errorf("write secret key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, false, fmt.Errorf("sync secret key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, false, fmt.Errorf("close secret key: %w", err)
	}
	if err := syncSecretDirectory(filepath.Dir(path)); err != nil {
		return nil, false, err
	}
	remove = false
	box, err := NewSecretBox(key, randomSource)
	return box, true, err
}

func LoadSecretBox(path string, randomSource io.Reader) (*SecretBox, error) {
	key, err := readSecretKey(path)
	if err != nil {
		return nil, err
	}
	defer wipe(key)
	return NewSecretBox(key, randomSource)
}

func readSecretKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("secret key must be a private regular file with mode 0600")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret key: %w", err)
	}
	if len(contents) != SecretKeySize {
		wipe(contents)
		return nil, fmt.Errorf("secret key must contain exactly %d bytes", SecretKeySize)
	}
	return contents, nil
}

func syncSecretDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open secret key directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync secret key directory: %w", err)
	}
	return nil
}

func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
