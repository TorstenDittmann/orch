package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The testdata signatures were made with `ssh-keygen -Y sign`, so this checks
// compatibility with OpenSSH, which install.sh uses to verify the same files.
func TestVerifyAcceptsAnSSHKeygenSignature(t *testing.T) {
	keys, err := ParseAllowedSigners(readTestdata(t, "allowed_signers"))
	if err != nil || len(keys) != 1 {
		t.Fatalf("ParseAllowedSigners = %d keys, %v", len(keys), err)
	}
	sums := readTestdata(t, "SHA256SUMS")
	if err := Verify(keys, sums, readTestdata(t, "SHA256SUMS.sig")); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	tampered := []byte(strings.Replace(string(sums), "abc123", "abc124", 1))
	if err := Verify(keys, tampered, readTestdata(t, "SHA256SUMS.sig")); err == nil {
		t.Fatal("Verify() accepted changed SHA256SUMS")
	}
	if err := Verify(keys, sums, readTestdata(t, "other-key.sig")); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("Verify(other key) = %v", err)
	}
	if err := Verify(keys, sums, readTestdata(t, "wrong-namespace.sig")); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("Verify(wrong namespace) = %v", err)
	}
	if err := Verify(keys, sums, []byte("not a signature")); err == nil {
		t.Fatal("Verify() accepted garbage")
	}
}

func TestParseAllowedSignersFiltersIdentityAndNamespace(t *testing.T) {
	line := strings.TrimSpace(string(readTestdata(t, "allowed_signers")))
	key := line[strings.Index(line, "ssh-ed25519"):]
	for input, want := range map[string]int{
		"# comment only\n":                                        0,
		"orch-release " + key:                                     1,
		"someone-else " + key:                                     0,
		`orch-release namespaces="file" ` + key:                   0,
		`orch-release,other namespaces="git,orch-release" ` + key: 1,
	} {
		keys, err := ParseAllowedSigners([]byte(input))
		if err != nil || len(keys) != want {
			t.Errorf("ParseAllowedSigners(%q) = %d, %v; want %d", input, len(keys), err, want)
		}
	}
}

func TestSignRoundTripsThroughVerify(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("sums\n")
	signature, err := Sign(signer, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify([]ssh.PublicKey{signer.PublicKey()}, message, signature); err != nil {
		t.Fatalf("Verify(Sign()) = %v", err)
	}
}
