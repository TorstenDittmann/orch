package control

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/security"
)

// fakeGitHub serves a JWKS and mints tokens signed like GitHub Actions.
type fakeGitHub struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeGitHub{key: key}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "test-key", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeGitHub) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	base := map[string]any{
		"iss": githubActionsIssuer, "exp": time.Now().Add(5 * time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
		"repository": "Acme/Hello", "ref": "refs/heads/main", "sha": "abc", "actor": "octocat", "run_id": "1",
	}
	for key, value := range claims {
		if value == nil {
			delete(base, key)
		} else {
			base[key] = value
		}
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	payload, _ := json.Marshal(base)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestGitHubVerifierChecksSignatureIssuerAudienceAndExpiry(t *testing.T) {
	fake := newFakeGitHub(t)
	verifier := newGitHubVerifier(time.Now)
	verifier.jwksURL = fake.server.URL
	ctx := context.Background()
	claims, err := verifier.verify(ctx, fake.token(t, map[string]any{"aud": "orch:one"}), "orch:one")
	if err != nil || claims.Repository != "Acme/Hello" || claims.Ref != "refs/heads/main" {
		t.Fatalf("valid token: %+v %v", claims, err)
	}
	if _, err := verifier.verify(ctx, fake.token(t, map[string]any{"aud": []string{"other", "orch:one"}}), "orch:one"); err != nil {
		t.Fatalf("audience list: %v", err)
	}
	for name, claims := range map[string]map[string]any{
		"wrong audience": {"aud": "orch:two"},
		"no audience":    {},
		"wrong issuer":   {"aud": "orch:one", "iss": "https://example.com"},
		"expired":        {"aud": "orch:one", "exp": time.Now().Add(-2 * time.Minute).Unix()},
		"not yet valid":  {"aud": "orch:one", "nbf": time.Now().Add(5 * time.Minute).Unix()},
		"no repository":  {"aud": "orch:one", "repository": nil},
	} {
		if _, err := verifier.verify(ctx, fake.token(t, claims), "orch:one"); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
	tampered := fake.token(t, map[string]any{"aud": "orch:one"})
	tampered = tampered[:len(tampered)-4] + "AAAA"
	if _, err := verifier.verify(ctx, tampered, "orch:one"); err == nil {
		t.Error("tampered signature accepted")
	}
}

func TestGitHubTrustIssuesCredentialsLimitedToOneApplication(t *testing.T) {
	fake := newFakeGitHub(t)
	_, dataDir, contextPath, clusterID := bootstrapController(t)
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0", GitHubKeysURL: fake.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	endpoint := "https://" + server.Addr().String()
	rewriteContextServer(t, contextPath, endpoint)
	credentials, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.CloseIdleConnections)
	ctx := context.Background()
	trusted, err := admin.SetGitHubTrust(ctx, "Acme/Hello", "hello", "main", "")
	if err != nil || !trusted.GetChanged() || trusted.GetTrust().GetRepository() != "acme/hello" || trusted.GetClusterId() != clusterID || len(trusted.GetCaFingerprint()) != sha256.Size {
		t.Fatalf("SetGitHubTrust() = %+v, %v", trusted, err)
	}
	if again, err := admin.SetGitHubTrust(ctx, "acme/hello", "hello", "main", ""); err != nil || again.GetChanged() {
		t.Fatalf("repeated trust = %+v, %v", again, err)
	}
	if _, err := admin.SetGitHubTrust(ctx, "not a repo", "hello", "main", ""); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid repository = %v", err)
	}

	exchange := orchv1alpha1connect.NewCredentialServiceClient(tlsHTTPClient(credentials.RootCAs, tls.Certificate{}), endpoint)
	audience := GitHubAudience(clusterID)
	issue := func(claims map[string]any, application string) (*orchv1alpha1.ExchangeGitHubTokenResponse, []byte, error) {
		claims["aud"] = audience
		csr, key, err := security.GenerateDeployerCertificateRequest(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		response, err := exchange.ExchangeGitHubToken(ctx, connect.NewRequest(&orchv1alpha1.ExchangeGitHubTokenRequest{
			Token: fake.token(t, claims), Application: application, CertificateRequestDer: csr,
		}))
		if err != nil {
			return nil, nil, err
		}
		return response.Msg, key, nil
	}
	if _, _, err := issue(map[string]any{"ref": "refs/heads/feature"}, ""); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("other branch = %v", err)
	}
	if _, _, err := issue(map[string]any{"event_name": "pull_request_target"}, ""); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("pull_request_target = %v", err)
	}
	if _, _, err := issue(map[string]any{"repository": "acme/other"}, ""); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("untrusted repository = %v", err)
	}
	if _, _, err := issue(map[string]any{}, "other"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("untrusted application = %v", err)
	}
	if _, _, err := issue(map[string]any{"iss": "https://example.com"}, ""); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("foreign issuer = %v", err)
	}
	issued, keyPEM, err := issue(map[string]any{}, "")
	if err != nil || issued.GetApplication() != "hello" || time.Until(issued.GetExpiresAt().AsTime()) > security.DeployerCertificateLifetime {
		t.Fatalf("exchange = %+v, %v", issued, err)
	}
	pair, err := tls.X509KeyPair(issued.GetCertificatePem(), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	deployer := generatedClient(tlsHTTPClient(credentials.RootCAs, pair), endpoint)
	if _, err := deployer.GetCluster(ctx, connect.NewRequest(&orchv1alpha1.GetClusterRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer GetCluster = %v", err)
	}
	if _, err := deployer.ListGitHubTrusts(ctx, connect.NewRequest(&orchv1alpha1.ListGitHubTrustsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer ListGitHubTrusts = %v", err)
	}
	other := mustCanonicalConfig(t, newTestManifest("other", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	}))
	if _, err := deployer.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: other, IdempotencyKey: "deployer-other-key-1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deploy other application = %v", err)
	}
	hello := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	}))
	applied, err := deployer.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: hello, IdempotencyKey: "deployer-hello-key-1"}))
	if err != nil || applied.Msg.GetName() != "hello" {
		t.Fatalf("deploy own application = %+v, %v", applied, err)
	}
	if _, err := deployer.GetApplication(ctx, connect.NewRequest(&orchv1alpha1.GetApplicationRequest{Name: "hello"})); err != nil {
		t.Fatalf("deployer GetApplication = %v", err)
	}
	if _, err := deployer.ListDeployments(ctx, connect.NewRequest(&orchv1alpha1.ListDeploymentsRequest{Application: "other", Limit: 5})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer history of other application = %v", err)
	}

	if removed, err := admin.DeleteGitHubTrust(ctx, "acme/hello", "hello"); err != nil || !removed {
		t.Fatalf("DeleteGitHubTrust() = %v, %v", removed, err)
	}
	if _, _, err := issue(map[string]any{}, ""); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("exchange after untrust = %v", err)
	}
}
