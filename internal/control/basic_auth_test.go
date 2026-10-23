package control

import (
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
	"golang.org/x/crypto/argon2"
)

func TestBasicAuthHashIsStablePerSecretVersion(t *testing.T) {
	secret := model.ResolvedSecret{Application: "site", Name: "admin-password", VersionID: "v1", Value: []byte("hunter2")}
	first := basicAuthHash(secret)
	basicAuthHashes.Delete("site\x00admin-password\x00v1")
	if again := basicAuthHash(secret); again != first {
		t.Fatalf("hash changed between plans: %q != %q", again, first)
	}
	rotated := secret
	rotated.VersionID = "v2"
	if basicAuthHash(rotated) == first {
		t.Fatal("a new secret version reused the old salt")
	}
	if strings.Contains(first, "hunter2") {
		t.Fatal("hash contains the password")
	}

	// Verify the way Caddy's argon2id module does.
	parts := strings.Split(first, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=47104,t=1,p=1" {
		t.Fatalf("hash = %q, want an argon2id PHC string", first)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatal(err)
	}
	stored, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatal(err)
	}
	if subtle.ConstantTimeCompare(argon2.IDKey([]byte("hunter2"), salt, 1, 47104, 1, uint32(len(stored))), stored) != 1 {
		t.Fatal("hash does not verify the password")
	}
	if subtle.ConstantTimeCompare(argon2.IDKey([]byte("wrong"), salt, 1, 47104, 1, uint32(len(stored))), stored) == 1 {
		t.Fatal("hash verifies a wrong password")
	}
}
