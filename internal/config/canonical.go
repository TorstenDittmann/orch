package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

func CanonicalJSON(manifest *Manifest) ([]byte, error) {
	if manifest == nil {
		return nil, fmt.Errorf("canonicalize manifest: manifest is nil")
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("canonicalize manifest: %w", err)
	}
	return append(data, '\n'), nil
}

func Digest(manifest *Manifest) (string, error) {
	data, err := CanonicalJSON(manifest)
	if err != nil {
		return "", err
	}
	return DigestCanonicalJSON(data), nil
}

// DigestCanonicalJSON hashes bytes that have already passed
// ParseCanonicalJSON or were returned by CanonicalJSON.
func DigestCanonicalJSON(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
