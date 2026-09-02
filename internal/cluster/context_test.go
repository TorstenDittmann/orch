package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadContextIsStrictAndRejectsUnsafePaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		body   string
		needle string
	}{
		{
			name:   "unknown field",
			body:   `{"version":"v1alpha1","cluster":{"id":"id","name":"test"},"server":"https://example","credentials":{"certificate_authority":"identities/id/ca.pem","client_certificate":"identities/id/admin.pem","client_key":"identities/id/admin-key.pem"},"extra":true}`,
			needle: "unknown field",
		},
		{
			name:   "absolute credential",
			body:   `{"version":"v1alpha1","cluster":{"id":"id","name":"test"},"server":"https://example","credentials":{"certificate_authority":"/tmp/ca.pem","client_certificate":"identities/id/admin.pem","client_key":"identities/id/admin-key.pem"}}`,
			needle: "clean relative path",
		},
		{
			name:   "traversal",
			body:   `{"version":"v1alpha1","cluster":{"id":"id","name":"test"},"server":"https://example","credentials":{"certificate_authority":"../ca.pem","client_certificate":"identities/id/admin.pem","client_key":"identities/id/admin-key.pem"}}`,
			needle: "clean relative path",
		},
		{
			name:   "trailing value",
			body:   `{"version":"v1alpha1","cluster":{"id":"id","name":"test"},"server":"https://example","credentials":{"certificate_authority":"identities/id/ca.pem","client_certificate":"identities/id/admin.pem","client_key":"identities/id/admin-key.pem"}} {}`,
			needle: "more than one JSON value",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "context.json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadContext(path); err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("LoadContext() = %v, want %q", err, test.needle)
			}
		})
	}
}

func TestLoadContextRejectsGroupReadableFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(path, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContext(path); err == nil || !strings.Contains(err.Error(), "group or world access") {
		t.Fatalf("LoadContext() = %v, want permission error", err)
	}
}
