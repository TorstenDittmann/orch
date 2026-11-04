package config

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	pklschema "github.com/TorstenDittmann/orch/pkl"
	applepkl "github.com/apple/pkl-go/pkl"
)

// ModuleLockFile records the SHA-256 of every remote Pkl module a manifest
// imports. It sits next to the manifest and is written on first use, like a
// go.sum: commit it, and a module that changes on its server afterwards stops
// the evaluation instead of changing what gets deployed.
const ModuleLockFile = "orch.lock"

const moduleLockHeader = "# Remote Pkl modules this folder's manifests import, pinned on first use.\n" +
	"# Commit this file. To accept a changed module, delete its line and deploy again.\n"

// pinnedModules reads https: modules in place of Pkl, so their contents can
// be checked against the lock file.
type pinnedModules struct {
	ctx    context.Context
	path   string
	client *http.Client

	mu      sync.Mutex
	pins    map[string]string
	changed bool
}

func newPinnedModules(ctx context.Context, manifest string, caCertificates []byte) (*pinnedModules, error) {
	modules := &pinnedModules{ctx: ctx, path: filepath.Join(filepath.Dir(manifest), ModuleLockFile), pins: make(map[string]string)}
	contents, err := os.ReadFile(modules.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", ModuleLockFile, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "https://") || !validModuleDigest(fields[1]) {
			return nil, fmt.Errorf("%s line %d: want a URL and its sha256:HEX digest", ModuleLockFile, line)
		}
		modules.pins[fields[0]] = fields[1]
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(caCertificates) != 0 {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caCertificates) {
			return nil, errors.New("invalid CA certificates for Pkl modules")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	modules.client = &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 || request.URL.Scheme != "https" {
				return errors.New("remote Pkl modules may only redirect to HTTPS, at most 5 times")
			}
			return nil
		}}
	return modules, nil
}

func validModuleDigest(value string) bool {
	encoded, ok := strings.CutPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(encoded)
	return ok && err == nil && len(decoded) == sha256.Size
}

func (*pinnedModules) Scheme() string            { return "https" }
func (*pinnedModules) IsGlobbable() bool         { return false }
func (*pinnedModules) HasHierarchicalUris() bool { return true }
func (*pinnedModules) IsLocal() bool             { return false }

func (*pinnedModules) ListElements(url.URL) ([]applepkl.PathElement, error) {
	return nil, errors.New("remote Pkl modules cannot be listed")
}

func (m *pinnedModules) Read(uri url.URL) (string, error) {
	raw := uri.String()
	// The orch schema always comes from this binary, offline.
	for _, base := range []string{pklschema.SchemaBaseURL, pklschema.LegacySchemaBaseURL} {
		if raw == base+"v1alpha1/Application.pkl" {
			return string(pklschema.ApplicationSource()), nil
		}
	}
	contents, err := m.fetch(raw)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", raw, err)
	}
	sum := sha256.Sum256(contents)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	pinned, ok := m.pins[raw]
	if ok && pinned != digest {
		return "", fmt.Errorf("remote module %s changed since %s pinned it (%s, now %s); if you expect the change, delete its line from %s and deploy again",
			raw, m.path, pinned, digest, m.path)
	}
	if !ok {
		m.pins[raw], m.changed = digest, true
	}
	return string(contents), nil
}

func (m *pinnedModules) fetch(raw string) ([]byte, error) {
	request, err := http.NewRequestWithContext(m.ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	response, err := m.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server answered %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) > MaxManifestBytes {
		return nil, fmt.Errorf("module exceeds %d bytes", MaxManifestBytes)
	}
	return contents, nil
}

// save writes the lock file when this evaluation pinned new modules. A
// folder that can't be written only loses the pins, not the deploy.
func (m *pinnedModules) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.changed {
		return
	}
	var contents strings.Builder
	contents.WriteString(moduleLockHeader)
	for _, raw := range slices.Sorted(maps.Keys(m.pins)) {
		contents.WriteString(raw + " " + m.pins[raw] + "\n")
	}
	temporary, err := os.CreateTemp(filepath.Dir(m.path), ".orch-lock-*")
	if err != nil {
		return
	}
	defer os.Remove(temporary.Name())
	_, err = temporary.WriteString(contents.String())
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil && os.Chmod(temporary.Name(), 0o644) == nil {
		_ = os.Rename(temporary.Name(), m.path)
	}
}
