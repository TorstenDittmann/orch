package client

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/control"
)

func TestParseClusterAddress(t *testing.T) {
	fingerprint := sha256.Sum256([]byte("ca"))
	address := ClusterAddress("https://203.0.113.10:9443", fingerprint[:])
	endpoint, parsed, err := ParseClusterAddress(address)
	if err != nil || endpoint != "https://203.0.113.10:9443" || string(parsed) != string(fingerprint[:]) {
		t.Fatalf("ParseClusterAddress(%q) = %q %x %v", address, endpoint, parsed, err)
	}
	for _, invalid := range []string{"https://203.0.113.10:9443", "http://host#sha256:" + strings.Repeat("a", 64), "https://host/path#sha256:" + strings.Repeat("a", 64), "https://host#sha256:abc"} {
		if _, _, err := ParseClusterAddress(invalid); err == nil {
			t.Errorf("ParseClusterAddress(%q) succeeded", invalid)
		}
	}
}

func TestGitHubActionsTokenRequiresIDTokenPermission(t *testing.T) {
	if _, err := githubActionsToken(context.Background(), func(string) string { return "" }, "orch:one"); err == nil || !strings.Contains(err.Error(), "id-token: write") {
		t.Fatalf("missing OIDC env = %v", err)
	}
}

func TestNewGitHubActionsDeploysOnlyTheTrustedApplication(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(keys.Close)
	tokens := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runtime-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
		payload, _ := json.Marshal(map[string]any{
			"iss": "https://token.actions.githubusercontent.com", "aud": r.URL.Query().Get("audience"),
			"exp": time.Now().Add(5 * time.Minute).Unix(), "repository": "acme/hello", "ref": "refs/heads/main",
		})
		input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		digest := sha256.Sum256([]byte(input))
		signature, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		_ = json.NewEncoder(w).Encode(map[string]string{"value": input + "." + base64.RawURLEncoding.EncodeToString(signature)})
	}))
	t.Cleanup(tokens.Close)
	previous := githubTokenClient
	githubTokenClient = tokens.Client()
	t.Cleanup(func() { githubTokenClient = previous })
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", tokens.URL+"/token?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runtime-token")

	root := t.TempDir()
	contextPath := filepath.Join(root, "config", "context.json")
	dataDir := filepath.Join(root, "state")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1", DataDir: dataDir, ContextPath: contextPath, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := control.Start(context.Background(), control.Options{DataDir: dataDir, Listen: "127.0.0.1:0", GitHubKeysURL: keys.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	endpoint := "https://" + server.Addr().String()
	local, err := clusterctl.LoadContext(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	local.Server = endpoint
	contents, _ := json.Marshal(local)
	if err := os.WriteFile(contextPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	admin, err := New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.CloseIdleConnections)
	ctx := context.Background()
	trust, err := admin.SetGitHubTrust(ctx, "acme/hello", "hello", "main", "")
	if err != nil {
		t.Fatal(err)
	}

	deployer, err := NewGitHubActions(ctx, ClusterAddress(endpoint, trust.GetCaFingerprint()), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deployer.CloseIdleConnections)
	data, err := os.ReadFile("../config/testdata/hello.canonical.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := config.ParseCanonicalJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.OverrideImages(manifest, []string{"ghcr.io/acme/hello@sha256:" + strings.Repeat("1", 64)}); err != nil {
		t.Fatal(err)
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deployer.ApplyApplication(ctx, canonical, "github-actions-deploy-1"); err != nil {
		t.Fatalf("deploy trusted application: %v", err)
	}
	if _, err := deployer.ListNodes(ctx); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer ListNodes = %v", err)
	}
	fingerprint := trust.GetCaFingerprint()
	fingerprint[0] ^= 1
	if _, err := NewGitHubActions(ctx, ClusterAddress(endpoint, fingerprint), ""); err == nil {
		t.Fatal("wrong CA fingerprint was accepted")
	}
}
