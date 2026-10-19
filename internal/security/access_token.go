package security

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// accessTokenPrefix starts every ORCH_TOKEN value so it is easy to recognize
// in a leaked log or a secret scanner.
const accessTokenPrefix = "orch_"

// NewAccessTokenSecret returns a random token ID and secret, and the hex
// SHA-256 of the secret that the controller stores instead of the secret.
func NewAccessTokenSecret(random io.Reader) (id, secret, secretHash string, err error) {
	var idBytes [8]byte
	var secretBytes [32]byte
	if _, err := io.ReadFull(random, idBytes[:]); err != nil {
		return "", "", "", fmt.Errorf("generate access token id: %w", err)
	}
	if _, err := io.ReadFull(random, secretBytes[:]); err != nil {
		return "", "", "", fmt.Errorf("generate access token secret: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(secretBytes[:])
	return hex.EncodeToString(idBytes[:]), secret, AccessTokenSecretHash(secret), nil
}

// AccessTokenSecretHash is the stored form of a token secret.
func AccessTokenSecretHash(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

// EncodeAccessToken builds the single ORCH_TOKEN value: where the cluster
// is, which CA to pin, and the credential, so nothing else is needed.
func EncodeAccessToken(clusterAddress, id, secret string) string {
	return accessTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(clusterAddress)) + "." + id + "." + secret
}

// ParseAccessToken splits an ORCH_TOKEN value into its cluster address and
// the "ID.SECRET" credential the controller exchanges.
func ParseAccessToken(value string) (clusterAddress, credential string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(value), accessTokenPrefix)
	parts := strings.Split(rest, ".")
	invalid := errors.New("ORCH_TOKEN is not an orch access token; create one with orch token create")
	if !ok || len(parts) != 3 {
		return "", "", invalid
	}
	address, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(address) == 0 {
		return "", "", invalid
	}
	id, secret, ok := SplitAccessTokenCredential(parts[1] + "." + parts[2])
	if !ok {
		return "", "", invalid
	}
	return string(address), id + "." + secret, nil
}

// SplitAccessTokenCredential validates and splits "ID.SECRET".
func SplitAccessTokenCredential(credential string) (id, secret string, ok bool) {
	id, secret, ok = strings.Cut(credential, ".")
	if !ok || len(id) != 16 || len(secret) != 43 {
		return "", "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", "", false
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(decoded) != 32 {
		return "", "", false
	}
	return id, secret, true
}
