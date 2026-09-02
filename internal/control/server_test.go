package control

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/security"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

const controlTestManifest = `schema = "v1alpha1"
name = "hello"

[services.web]
image = "example/web:1"
`

func TestControllerServesAuthenticatedDurableIntentAndHealth(t *testing.T) {
	root, dataDir, contextPath, clusterID := bootstrapController(t)
	_ = root
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
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
	healthClient := tlsHTTPClient(credentials.RootCAs, tls.Certificate{})
	response, err := healthClient.Get(endpoint + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok\n" || strings.Contains(string(body), clusterID) || strings.Contains(string(body), "test") {
		t.Fatalf("health response = %d %q", response.StatusCode, body)
	}

	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.CloseIdleConnections)
	cluster, err := remote.GetCluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cluster.GetId() != clusterID || cluster.GetName() != "test" {
		t.Fatalf("cluster = %#v", cluster)
	}
	first, err := remote.ApplyApplication(context.Background(), []byte(controlTestManifest), "idempotency-key-one")
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetChanged() || first.GetGeneration() != 1 || first.GetEventSequence() != 2 || first.GetExecutionStatus() != orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING {
		t.Fatalf("first apply = %#v", first)
	}
	replay, err := remote.ApplyApplication(context.Background(), []byte("\n"+controlTestManifest), "idempotency-key-one")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(replay, first) {
		t.Fatalf("semantic replay = %#v, want %#v", replay, first)
	}
	noOp, err := remote.ApplyApplication(context.Background(), []byte(controlTestManifest), "idempotency-key-two")
	if err != nil {
		t.Fatal(err)
	}
	if noOp.GetChanged() || noOp.GetGeneration() != 1 || noOp.GetRevisionId() != first.GetRevisionId() || noOp.GetEventSequence() != 0 {
		t.Fatalf("no-op apply = %#v", noOp)
	}
	conflictingManifest := strings.Replace(controlTestManifest, "example/web:1", "example/web:2", 1)
	_, err = remote.ApplyApplication(context.Background(), []byte(conflictingManifest), "idempotency-key-one")
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("idempotency conflict = %v, want already exists", err)
	}
	application, err := remote.GetApplication(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if application.GetDigest() != first.GetDigest() || !json.Valid([]byte(application.GetCanonicalManifestJson())) {
		t.Fatalf("application = %#v", application)
	}
	events, err := remote.ListEvents(context.Background(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events.GetEvents()) != 1 || events.GetEvents()[0].GetSequence() != 1 || events.GetNextAfterSequence() != 1 {
		t.Fatalf("first event page = %#v", events)
	}
	events, err = remote.ListEvents(context.Background(), events.GetNextAfterSequence(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events.GetEvents()) != 1 || events.GetEvents()[0].GetSequence() != 2 || events.GetNextAfterSequence() != 2 {
		t.Fatalf("second event page = %#v", events)
	}
	empty, err := remote.ListEvents(context.Background(), 99, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.GetEvents()) != 0 || empty.GetNextAfterSequence() != 99 {
		t.Fatalf("empty event page = %#v", empty)
	}
	_, err = remote.GetApplication(context.Background(), "Not Valid")
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid application lookup = %v, want invalid argument", err)
	}
	_, err = remote.GetApplication(context.Background(), "missing")
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing application lookup = %v, want not found", err)
	}
	_, err = remote.ApplyApplication(context.Background(), []byte("not = [valid"), "idempotency-key-bad-manifest")
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid manifest = %v, want invalid argument", err)
	}
	_, err = remote.ApplyApplication(context.Background(), bytes.Repeat([]byte("x"), maxManifestBytes+1), "idempotency-key-too-large")
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized manifest = %v, want resource exhausted", err)
	}
}

func TestControllerRequiresAdministratorIdentity(t *testing.T) {
	_, dataDir, contextPath, clusterID := bootstrapController(t)
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
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

	withoutCertificate := generatedClient(tlsHTTPClient(credentials.RootCAs, tls.Certificate{}), endpoint)
	_, err = withoutCertificate.GetCluster(context.Background(), connect.NewRequest(&orchv1alpha1.GetClusterRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("certificate-free request = %v, want unauthenticated", err)
	}

	controllerPair, err := tls.LoadX509KeyPair(
		filepath.Join(dataDir, "pki", "controller.pem"),
		filepath.Join(dataDir, "pki", "controller-key.pem"),
	)
	if err != nil {
		t.Fatal(err)
	}
	wrongRole := generatedClient(tlsHTTPClient(credentials.RootCAs, controllerPair), endpoint)
	_, err = wrongRole.GetCluster(context.Background(), connect.NewRequest(&orchv1alpha1.GetClusterRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("controller-role request = %v, want permission denied", err)
	}

	duplicateURI := mintClientCertificate(t, dataDir, clusterID, func(certificate *x509.Certificate) {
		identity := security.IdentityURI(clusterID, security.RoleAdministrator)
		certificate.URIs = []*url.URL{identity, identity}
	})
	duplicateClient := generatedClient(tlsHTTPClient(credentials.RootCAs, duplicateURI), endpoint)
	_, err = duplicateClient.GetCluster(context.Background(), connect.NewRequest(&orchv1alpha1.GetClusterRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("duplicate-identity request = %v, want permission denied", err)
	}
	wrongCluster := mintClientCertificate(t, dataDir, "00000000-0000-4000-8000-000000000000", func(*x509.Certificate) {})
	wrongClusterClient := generatedClient(tlsHTTPClient(credentials.RootCAs, wrongCluster), endpoint)
	_, err = wrongClusterClient.GetCluster(context.Background(), connect.NewRequest(&orchv1alpha1.GetClusterRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("wrong-cluster request = %v, want permission denied", err)
	}

	expired := mintClientCertificate(t, dataDir, clusterID, func(certificate *x509.Certificate) {
		certificate.NotBefore = time.Now().Add(-2 * time.Hour)
		certificate.NotAfter = time.Now().Add(-time.Hour)
	})
	expiredClient := generatedClient(tlsHTTPClient(credentials.RootCAs, expired), endpoint)
	if _, err := expiredClient.GetCluster(context.Background(), connect.NewRequest(&orchv1alpha1.GetClusterRequest{})); err == nil {
		t.Fatal("expired administrator certificate was accepted")
	}

	foreignRoot := t.TempDir()
	foreignContext := filepath.Join(foreignRoot, "config", "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "foreign", Server: "https://127.0.0.1", DataDir: filepath.Join(foreignRoot, "state"),
		ContextPath: foreignContext, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	foreignCredentials, err := clusterctl.LoadAdministratorCredentials(foreignContext, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	foreignClient := generatedClient(tlsHTTPClient(credentials.RootCAs, foreignCredentials.Certificate), endpoint)
	if _, err := foreignClient.GetCluster(context.Background(), connect.NewRequest(&orchv1alpha1.GetClusterRequest{})); err == nil {
		t.Fatal("foreign-cluster certificate was accepted")
	}
}

func TestControllerExcludesConcurrentOwnerAndRestartsWithState(t *testing.T) {
	_, dataDir, contextPath, _ := bootstrapController(t)
	first, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"}); !errors.Is(err, ErrControllerRunning) {
		t.Fatalf("second Start() = %v, want ErrControllerRunning", err)
	}
	rewriteContextServer(t, contextPath, "https://"+first.Addr().String())
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	firstApply, err := remote.ApplyApplication(context.Background(), []byte(controlTestManifest), "idempotency-key-one")
	if err != nil {
		t.Fatal(err)
	}
	remote.CloseIdleConnections()
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	second, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	rewriteContextServer(t, contextPath, "https://"+second.Addr().String())
	restarted, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.CloseIdleConnections)
	replay, err := restarted.ApplyApplication(context.Background(), []byte(controlTestManifest), "idempotency-key-one")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(replay, firstApply) {
		t.Fatalf("restarted replay = %#v, want %#v", replay, firstApply)
	}
}

func TestControllerRefusesMismatchedIdentityAndReleasesLock(t *testing.T) {
	_, dataDir, _, _ := bootstrapController(t)
	controllerKey := filepath.Join(dataDir, "pki", "controller-key.pem")
	if err := os.WriteFile(controllerKey, []byte("not a private key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"}); err == nil || !strings.Contains(err.Error(), "controller private key") {
		t.Fatalf("Start() = %v, want identity error", err)
	}
	lock, err := acquireDataLock(dataDir)
	if err != nil {
		t.Fatalf("failed startup retained lock: %v", err)
	}
	defer lock.Close()
}

func TestControllerRejectsInsecureDatabaseBeforeOpening(t *testing.T) {
	_, dataDir, _, _ := bootstrapController(t)
	databasePath := filepath.Join(dataDir, clusterctl.DatabaseFilename)
	if err := os.Chmod(databasePath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"}); err == nil || !strings.Contains(err.Error(), "group or world access") {
		t.Fatalf("Start() = %v, want insecure database error", err)
	}
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("database permissions changed to %04o", info.Mode().Perm())
	}
}

func bootstrapController(t *testing.T) (root, dataDir, contextPath, clusterID string) {
	t.Helper()
	root = t.TempDir()
	dataDir = filepath.Join(root, "state")
	contextPath = filepath.Join(root, "config", "context.json")
	result, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1", DataDir: dataDir,
		ContextPath: contextPath, Now: time.Now, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	return root, dataDir, contextPath, result.Cluster.ID
}

func rewriteContextServer(t *testing.T, path, endpoint string) {
	t.Helper()
	local, err := clusterctl.LoadContext(path)
	if err != nil {
		t.Fatal(err)
	}
	local.Server = endpoint
	contents, err := json.MarshalIndent(local, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tlsHTTPClient(roots *x509.CertPool, certificate tls.Certificate) *http.Client {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "127.0.0.1"}
	if len(certificate.Certificate) != 0 {
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true},
		Timeout:   5 * time.Second,
	}
}

func generatedClient(httpClient *http.Client, endpoint string) orchv1alpha1connect.ControlServiceClient {
	return orchv1alpha1connect.NewControlServiceClient(httpClient, endpoint)
}

func mintClientCertificate(t *testing.T, dataDir, clusterID string, mutate func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	caPEM, err := os.ReadFile(filepath.Join(dataDir, "pki", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ := pem.Decode(caPEM)
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dataDir, "pki", "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKey := parsedKey.(*ecdsa.PrivateKey)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(922337203685477580),
		Subject:      pkix.Name{CommonName: "test administrator"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{security.IdentityURI(clusterID, security.RoleAdministrator)},
	}
	mutate(template)
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}
