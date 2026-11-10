// Package release verifies the SSH signature that every orch release carries
// over its SHA256SUMS. The format is OpenSSH's (PROTOCOL.sshsig), so the same
// file can be checked with `ssh-keygen -Y verify` on any machine.
package release

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"

	releasekeys "github.com/TorstenDittmann/orch/packaging/release"
	"golang.org/x/crypto/ssh"
)

// Namespace and Identity are the ssh-keygen -n and -I values releases are
// signed and verified with.
const (
	Namespace = "orch-release"
	Identity  = "orch-release"
)

// SignatureAsset is the release asset holding the signature over SHA256SUMS.
const SignatureAsset = "SHA256SUMS.sig"

// ErrUnsigned means no signing key is configured in this build, so there is
// nothing to check a signature against.
var ErrUnsigned = errors.New("this build has no release signing key")

// Keys returns the release signing keys embedded in this build.
func Keys() ([]ssh.PublicKey, error) {
	return ParseAllowedSigners(releasekeys.AllowedSigners)
}

// Configured reports whether this build carries a release signing key.
func Configured() bool {
	keys, err := Keys()
	return err == nil && len(keys) > 0
}

// ParseAllowedSigners reads the keys for Identity from an ssh-keygen
// allowed_signers file whose namespaces option, when present, allows Namespace.
func ParseAllowedSigners(data []byte) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for number, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		start := -1
		for index, field := range fields {
			if index > 0 && (strings.HasPrefix(field, "ssh-") || strings.HasPrefix(field, "ecdsa-") || strings.HasPrefix(field, "sk-")) {
				start = index
				break
			}
		}
		if start < 0 {
			return nil, fmt.Errorf("allowed_signers line %d has no key", number+1)
		}
		if !principalMatches(fields[0], Identity) {
			continue
		}
		if options := strings.Join(fields[1:start], " "); options != "" && !namespaceAllowed(options) {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[start:], " ")))
		if err != nil {
			return nil, fmt.Errorf("allowed_signers line %d: %w", number+1, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func principalMatches(principals, identity string) bool {
	for _, principal := range strings.Split(principals, ",") {
		if principal == identity || principal == "*" {
			return true
		}
	}
	return false
}

var namespacesOption = regexp.MustCompile(`(?:^|,)namespaces="([^"]*)"`)

func namespaceAllowed(options string) bool {
	match := namespacesOption.FindStringSubmatch(options)
	if match == nil {
		return true
	}
	for _, namespace := range strings.Split(match[1], ",") {
		if namespace == Namespace || namespace == "*" {
			return true
		}
	}
	return false
}

// VerifySums checks an armored SSH signature over message against this
// build's release keys. It returns ErrUnsigned when the build has none.
func VerifySums(message, armored []byte) error {
	keys, err := Keys()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return ErrUnsigned
	}
	return Verify(keys, message, armored)
}

type sshSignatureBlob struct {
	PublicKey     []byte
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Signature     []byte
}

type signedData struct {
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Hash          []byte
}

// Verify checks an armored SSH signature (ssh-keygen -Y sign -n orch-release)
// over message against keys.
func Verify(keys []ssh.PublicKey, message, armored []byte) error {
	block, _ := pem.Decode(bytes.TrimSpace(armored))
	if block == nil || block.Type != "SSH SIGNATURE" {
		return errors.New("release signature is not an SSH signature")
	}
	raw := block.Bytes
	if len(raw) < 10 || string(raw[:6]) != "SSHSIG" {
		return errors.New("release signature has no SSHSIG header")
	}
	if version := uint32(raw[6])<<24 | uint32(raw[7])<<16 | uint32(raw[8])<<8 | uint32(raw[9]); version != 1 {
		return fmt.Errorf("release signature version %d is unsupported", version)
	}
	var blob sshSignatureBlob
	if err := ssh.Unmarshal(raw[10:], &blob); err != nil {
		return fmt.Errorf("parse release signature: %w", err)
	}
	if blob.Namespace != Namespace {
		return fmt.Errorf("release signature namespace %q is not %q", blob.Namespace, Namespace)
	}
	var digest []byte
	switch blob.HashAlgorithm {
	case "sha512":
		sum := sha512.Sum512(message)
		digest = sum[:]
	case "sha256":
		sum := sha256.Sum256(message)
		digest = sum[:]
	default:
		return fmt.Errorf("release signature hash %q is unsupported", blob.HashAlgorithm)
	}
	signer, err := ssh.ParsePublicKey(blob.PublicKey)
	if err != nil {
		return fmt.Errorf("parse release signature key: %w", err)
	}
	trusted := false
	for _, key := range keys {
		if bytes.Equal(key.Marshal(), signer.Marshal()) {
			trusted = true
		}
	}
	if !trusted {
		return errors.New("release is signed by an unknown key")
	}
	var signature ssh.Signature
	if err := ssh.Unmarshal(blob.Signature, &signature); err != nil {
		return fmt.Errorf("parse release signature: %w", err)
	}
	data := append([]byte("SSHSIG"), ssh.Marshal(signedData{Namespace: blob.Namespace, Reserved: blob.Reserved, HashAlgorithm: blob.HashAlgorithm, Hash: digest})...)
	if err := signer.Verify(data, &signature); err != nil {
		return errors.New("release signature does not match SHA256SUMS")
	}
	return nil
}

// fingerprint is used in error messages and tests.
func fingerprint(key ssh.PublicKey) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(func() []byte { s := sha256.Sum256(key.Marshal()); return s[:] }())
}

// Sign makes an armored SSH signature over message in the format
// `ssh-keygen -Y sign -n orch-release` writes. Releases are signed with
// ssh-keygen in CI; this exists so tests can sign their own fixtures.
func Sign(signer ssh.Signer, message []byte) ([]byte, error) {
	digest := sha512.Sum512(message)
	data := append([]byte("SSHSIG"), ssh.Marshal(signedData{Namespace: Namespace, HashAlgorithm: "sha512", Hash: digest[:]})...)
	algorithm := ""
	if signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		algorithm = ssh.KeyAlgoRSASHA512
	}
	var signature *ssh.Signature
	var err error
	if algorithmSigner, ok := signer.(ssh.AlgorithmSigner); ok && algorithm != "" {
		signature, err = algorithmSigner.SignWithAlgorithm(nil, data, algorithm)
	} else {
		signature, err = signer.Sign(nil, data)
	}
	if err != nil {
		return nil, err
	}
	blob := append([]byte("SSHSIG\x00\x00\x00\x01"), ssh.Marshal(sshSignatureBlob{
		PublicKey: signer.PublicKey().Marshal(), Namespace: Namespace, HashAlgorithm: "sha512", Signature: ssh.Marshal(signature),
	})...)
	return pem.EncodeToMemory(&pem.Block{Type: "SSH SIGNATURE", Bytes: blob}), nil
}
