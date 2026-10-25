package security

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Backups copied to a bucket are encrypted to a recovery key so that reading
// the bucket does not hand out the cluster CA key and secret key. The
// controller only ever holds the public half; the operator keeps the
// private half and needs it to restore from the bucket.
const (
	recoveryKeyPrefix       = "orch-recovery-"
	recoveryPublicKeyPrefix = "orch-recovery-public-"
	encryptedBackupMagic    = "orch-encrypted-backup/v1\n"
	recoveryChunkSize       = 64 << 10
	recoveryInfo            = "orch backup recovery v1"
)

// ErrRecoveryKey is returned when a backup cannot be opened with the key.
var ErrRecoveryKey = errors.New("the recovery key does not open this backup")

// NewRecoveryKey returns a new recovery key and its public key, both as
// printable strings.
func NewRecoveryKey(random io.Reader) (private, public string, err error) {
	key, err := ecdh.X25519().GenerateKey(random)
	if err != nil {
		return "", "", fmt.Errorf("generate recovery key: %w", err)
	}
	return recoveryKeyPrefix + base64.RawURLEncoding.EncodeToString(key.Bytes()),
		recoveryPublicKeyPrefix + base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), nil
}

// ValidRecoveryPublicKey reports whether value is a recovery public key.
func ValidRecoveryPublicKey(value string) bool {
	_, err := parseRecoveryPublicKey(value)
	return err == nil
}

func parseRecoveryPublicKey(value string) (*ecdh.PublicKey, error) {
	encoded, ok := strings.CutPrefix(value, recoveryPublicKeyPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if !ok || err != nil {
		return nil, errors.New("not an orch recovery public key")
	}
	return ecdh.X25519().NewPublicKey(raw)
}

func parseRecoveryKey(value string) (*ecdh.PrivateKey, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, recoveryPublicKeyPrefix) {
		return nil, errors.New("that is the recovery public key; restores need the recovery key printed by orch backup destination set")
	}
	encoded, ok := strings.CutPrefix(value, recoveryKeyPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if !ok || err != nil {
		return nil, errors.New("not an orch recovery key; it starts with " + recoveryKeyPrefix)
	}
	return ecdh.X25519().NewPrivateKey(raw)
}

// IsEncryptedBackup reports whether reader starts with an encrypted backup
// header, without consuming it.
func IsEncryptedBackup(reader *bufio.Reader) bool {
	header, _ := reader.Peek(len(encryptedBackupMagic))
	return string(header) == encryptedBackupMagic
}

// EncryptBackup writes plaintext encrypted to the recovery public key:
// the header, an ephemeral X25519 key, then 64 KiB AES-256-GCM chunks whose
// nonce counts the chunk and marks the last one, so a truncated or
// reordered stream fails to open.
func EncryptBackup(destination io.Writer, plaintext io.Reader, publicKey string, random io.Reader) error {
	recipient, err := parseRecoveryPublicKey(publicKey)
	if err != nil {
		return err
	}
	ephemeral, err := ecdh.X25519().GenerateKey(random)
	if err != nil {
		return fmt.Errorf("generate backup key: %w", err)
	}
	aead, err := backupAEAD(ephemeral, recipient, ephemeral.PublicKey().Bytes(), recipient.Bytes())
	if err != nil {
		return err
	}
	if _, err := io.WriteString(destination, encryptedBackupMagic); err != nil {
		return err
	}
	if _, err := destination.Write(ephemeral.PublicKey().Bytes()); err != nil {
		return err
	}
	current := make([]byte, recoveryChunkSize)
	next := make([]byte, recoveryChunkSize)
	n, err := io.ReadFull(plaintext, current)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	for counter := uint64(0); ; counter++ {
		// A full chunk is the last one only if nothing follows it.
		last := n < recoveryChunkSize
		m := 0
		if !last {
			m, err = io.ReadFull(plaintext, next)
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				return err
			}
			last = m == 0
		}
		if _, err := destination.Write(aead.Seal(nil, chunkNonce(counter, last), current[:n], nil)); err != nil {
			return err
		}
		if last {
			return nil
		}
		current, next, n = next, current, m
	}
}

// DecryptBackup returns a reader of the plaintext of an encrypted backup.
// It fails on the first chunk that does not authenticate.
func DecryptBackup(source *bufio.Reader, recoveryKey string) (io.Reader, error) {
	key, err := parseRecoveryKey(recoveryKey)
	if err != nil {
		return nil, err
	}
	header := make([]byte, len(encryptedBackupMagic)+32)
	if _, err := io.ReadFull(source, header); err != nil || string(header[:len(encryptedBackupMagic)]) != encryptedBackupMagic {
		return nil, errors.New("not an encrypted orch backup")
	}
	ephemeral, err := ecdh.X25519().NewPublicKey(header[len(encryptedBackupMagic):])
	if err != nil {
		return nil, errors.New("encrypted backup header is invalid")
	}
	aead, err := backupAEAD(key, ephemeral, ephemeral.Bytes(), key.PublicKey().Bytes())
	if err != nil {
		return nil, ErrRecoveryKey
	}
	return &backupDecrypter{source: source, aead: aead, chunk: make([]byte, recoveryChunkSize+aead.Overhead())}, nil
}

func backupAEAD(private *ecdh.PrivateKey, public *ecdh.PublicKey, ephemeral, recipient []byte) (cipher.AEAD, error) {
	shared, err := private.ECDH(public)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, shared, append(append([]byte(nil), ephemeral...), recipient...), recoveryInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(counter uint64, last bool) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[3:11], counter)
	if last {
		nonce[11] = 1
	}
	return nonce
}

type backupDecrypter struct {
	source  *bufio.Reader
	aead    cipher.AEAD
	chunk   []byte
	pending bytes.Reader
	counter uint64
	done    bool
	err     error
}

func (d *backupDecrypter) Read(buffer []byte) (int, error) {
	for d.pending.Len() == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		d.err = d.nextChunk()
	}
	return d.pending.Read(buffer)
}

func (d *backupDecrypter) nextChunk() error {
	n, err := io.ReadFull(d.source, d.chunk)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		if errors.Is(err, io.EOF) {
			return errors.New("encrypted backup is truncated")
		}
		return err
	}
	last := n < len(d.chunk)
	if !last {
		_, peekErr := d.source.Peek(1)
		last = errors.Is(peekErr, io.EOF)
	}
	plaintext, err := d.aead.Open(d.chunk[:0:0], chunkNonce(d.counter, last), d.chunk[:n], nil)
	if err != nil {
		if d.counter == 0 {
			return ErrRecoveryKey
		}
		return errors.New("encrypted backup is damaged or truncated")
	}
	d.counter++
	d.done = last
	d.pending.Reset(plaintext)
	return nil
}
