package config

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	pklschema "github.com/TorstenDittmann/orch/pkl"
)

func TestPinnedModulesPinOnFirstUseAndRefuseChanges(t *testing.T) {
	var body atomic.Value
	body.Store("x = 1\n")
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	manifest := filepath.Join(t.TempDir(), "orch.pkl")
	module, _ := url.Parse(server.URL + "/lib/base.pkl")
	read := func() (string, error) {
		t.Helper()
		modules, err := newPinnedModules(context.Background(), manifest, certificate)
		if err != nil {
			t.Fatal(err)
		}
		contents, err := modules.Read(*module)
		if err == nil {
			modules.save()
		}
		return contents, err
	}
	if contents, err := read(); err != nil || contents != "x = 1\n" {
		t.Fatalf("first read = %q, %v", contents, err)
	}
	lock, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), ModuleLockFile))
	if err != nil || !strings.Contains(string(lock), module.String()+" sha256:") {
		t.Fatalf("lock file = %q, %v; want the module pinned", lock, err)
	}
	if _, err := read(); err != nil {
		t.Fatalf("unchanged module refused: %v", err)
	}
	body.Store("x = 2\n")
	if _, err := read(); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("changed module = %v, want refused", err)
	}
	// Deleting the line accepts the new contents.
	if err := os.WriteFile(filepath.Join(filepath.Dir(manifest), ModuleLockFile), []byte(moduleLockHeader), 0o644); err != nil {
		t.Fatal(err)
	}
	if contents, err := read(); err != nil || contents != "x = 2\n" {
		t.Fatalf("read after unpinning = %q, %v", contents, err)
	}

	// The orch schema comes from the binary without a request or a pin.
	schema, _ := url.Parse(pklschema.ApplicationURL)
	modules, err := newPinnedModules(context.Background(), filepath.Join(t.TempDir(), "orch.pkl"), certificate)
	if err != nil {
		t.Fatal(err)
	}
	if contents, err := modules.Read(*schema); err != nil || contents != string(pklschema.ApplicationSource()) || modules.changed {
		t.Fatalf("schema read = %v, pinned %t", err, modules.changed)
	}

	if err := os.WriteFile(filepath.Join(filepath.Dir(manifest), ModuleLockFile), []byte("https://example.com/a.pkl md5:abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newPinnedModules(context.Background(), manifest, nil); err == nil {
		t.Fatal("a malformed lock file was accepted")
	}
}
