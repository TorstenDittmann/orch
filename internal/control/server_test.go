package control

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
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
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
	"change.codes/torsten/better-swarm/internal/target"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestJoinEnrollmentIsTheOnlyCertificateFreeControlSurface(t *testing.T) {
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
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.CloseIdleConnections)
	secret := bytes.Repeat([]byte{7}, 32)
	secretHash := sha256.Sum256(secret)
	created, err := remote.CreateJoinToken(context.Background(), &orchv1alpha1.CreateJoinTokenRequest{Id: "join_0123456789abcdef0123456789abcdef", NodeName: "node-1", SecretHash: secretHash[:], ExpiresAt: timestamppb.New(time.Now().Add(15 * time.Minute)), IdempotencyKey: "join-create-key-one"})
	if err != nil || created.GetToken().GetState() != model.JoinTokenPending {
		t.Fatalf("CreateJoinToken() = %#v, %v", created, err)
	}
	httpClient := tlsHTTPClient(credentials.RootCAs, tls.Certificate{})
	enrollment := orchv1alpha1connect.NewEnrollmentServiceClient(httpClient, endpoint)
	csr, _, _, err := security.GenerateAgentCertificateRequest("node-1", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := &orchv1alpha1.EnrollmentServiceEnrollAgentRequest{JoinId: created.GetToken().GetId(), Secret: bytes.Repeat([]byte{8}, 32), Name: "node-1", CertificateRequestDer: csr, IdempotencyKey: "joined-enroll-key-one"}
	if _, err := enrollment.EnrollAgent(context.Background(), connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("wrong join secret = %v", err)
	}
	request.Secret = secret
	response, err := enrollment.EnrollAgent(context.Background(), connect.NewRequest(request))
	if err != nil || response.Msg.GetClusterId() != clusterID || response.Msg.GetNodeName() != "node-1" {
		t.Fatalf("joined enrollment = %#v, %v", response, err)
	}
	replay, err := enrollment.EnrollAgent(context.Background(), connect.NewRequest(request))
	if err != nil || !proto.Equal(replay.Msg, response.Msg) {
		t.Fatalf("joined replay = %#v, %v", replay, err)
	}
	request.CertificateRequestDer = append([]byte(nil), csr...)
	request.CertificateRequestDer[len(request.CertificateRequestDer)-1] ^= 1
	if _, err := enrollment.EnrollAgent(context.Background(), connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("changed consumed request = %v", err)
	}
	tokens, err := remote.ListJoinTokens(context.Background())
	if err != nil || len(tokens) != 1 || tokens[0].GetState() != model.JoinTokenConsumed {
		t.Fatalf("consumed join metadata = %#v, %v", tokens, err)
	}
	events, err := remote.ListEvents(context.Background(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	encodedSecret := base64.StdEncoding.EncodeToString(secret)
	for _, event := range events.GetEvents() {
		if bytes.Contains([]byte(event.GetPayloadJson()), secret) || strings.Contains(event.GetPayloadJson(), encodedSecret) {
			t.Fatalf("event %s contains join secret", event.GetKind())
		}
	}
	withoutCertificate := generatedClient(httpClient, endpoint)
	if _, err := withoutCertificate.ListJoinTokens(context.Background(), connect.NewRequest(&orchv1alpha1.ListJoinTokensRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("certificate-free token list = %v", err)
	}
}

func TestWireGuardDialOverridePreservesCanonicalTLSIdentity(t *testing.T) {
	_, dataDir, contextPath, _ := bootstrapController(t)
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	remote, err := controlclient.NewEndpoint(controlclient.Endpoint{
		ContextPath: contextPath,
		Transport:   target.Transport{Type: target.TransportWG, Address: server.Addr().String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.CloseIdleConnections)
	cluster, err := remote.GetCluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cluster.GetName() != "test" {
		t.Fatalf("cluster name = %q", cluster.GetName())
	}
}

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
	unknownRequest := &orchv1alpha1.GetClusterRequest{}
	unknownRequest.ProtoReflect().SetUnknown([]byte{0x08, 0x01})
	credentialsForUnknown, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	unknownClient := generatedClient(tlsHTTPClient(credentialsForUnknown.RootCAs, credentialsForUnknown.Certificate), endpoint)
	_, err = unknownClient.GetCluster(context.Background(), connect.NewRequest(unknownRequest))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown request field = %v, want invalid argument", err)
	}
	legacyRequest := &orchv1alpha1.ApplyApplicationRequest{
		ManifestToml: []byte("legacy"), IdempotencyKey: "idempotency-key-legacy",
	}
	_, err = unknownClient.ApplyApplication(context.Background(), connect.NewRequest(legacyRequest))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("legacy manifest = %v, want migration error", err)
	}
	emptyRequest := &orchv1alpha1.ApplyApplicationRequest{IdempotencyKey: "idempotency-key-empty"}
	_, err = unknownClient.ApplyApplication(context.Background(), connect.NewRequest(emptyRequest))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "canonical_manifest_json is required") {
		t.Fatalf("empty manifest = %v, want required-field error", err)
	}

	manifest := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	}))
	first, err := remote.ApplyApplication(context.Background(), manifest, "idempotency-key-one")
	if err != nil {
		t.Fatal(err)
	}
	if !first.GetChanged() || first.GetGeneration() != 1 || first.GetEventSequence() != 2 || first.GetExecutionStatus() != orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_PENDING {
		t.Fatalf("first apply = %#v", first)
	}
	replay, err := remote.ApplyApplication(context.Background(), manifest, "idempotency-key-one")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(replay, first) {
		t.Fatalf("semantic replay = %#v, want %#v", replay, first)
	}
	noOp, err := remote.ApplyApplication(context.Background(), manifest, "idempotency-key-two")
	if err != nil {
		t.Fatal(err)
	}
	if noOp.GetChanged() || noOp.GetGeneration() != 1 || noOp.GetRevisionId() != first.GetRevisionId() || noOp.GetEventSequence() != 0 {
		t.Fatalf("no-op apply = %#v", noOp)
	}
	conflictingManifest := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"web": {Image: "example/web@sha256:2222222222222222222222222222222222222222222222222222222222222222"},
	}))
	_, err = remote.ApplyApplication(context.Background(), conflictingManifest, "idempotency-key-one")
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
	applications, err := remote.ListApplications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(applications) != 1 || applications[0].GetName() != "hello" || applications[0].GetGeneration() != 1 || applications[0].GetRevisionId() != first.GetRevisionId() {
		t.Fatalf("application summaries = %#v", applications)
	}
	runtimes, err := remote.ListRuntimes(context.Background(), 0)
	if err != nil || len(runtimes.GetRuntimes()) != 0 || runtimes.GetTruncated() {
		t.Fatalf("unplaced runtime inventory = %#v, %v", runtimes, err)
	}
	_, err = unknownClient.ListRuntimes(context.Background(), connect.NewRequest(&orchv1alpha1.ListRuntimesRequest{Limit: maxRuntimeLimit + 1}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversized runtime inventory = %v, want invalid argument", err)
	}
	recent, err := remote.ListRecentEvents(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent.GetEvents()) != 1 || recent.GetEvents()[0].GetSequence() != 2 || recent.GetNextAfterSequence() != 2 {
		t.Fatalf("recent event page = %#v", recent)
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
	invalidCanonical := map[string][]byte{
		"trailing": append(append([]byte(nil), manifest...), []byte("{}")...),
		"unknown JSON field": bytes.Replace(
			manifest,
			[]byte("{\n"),
			[]byte("{\n  \"unknown\": true,\n"),
			1,
		),
		"semantically invalid": bytes.Replace(
			manifest,
			[]byte(`"name": "hello"`),
			[]byte(`"name": "Not-Valid"`),
			1,
		),
	}
	for name, document := range invalidCanonical {
		t.Run(name, func(t *testing.T) {
			key := "idempotency-key-invalid-" + strings.ReplaceAll(name, " ", "-")
			_, applyErr := remote.ApplyApplication(context.Background(), document, key)
			if connect.CodeOf(applyErr) != connect.CodeInvalidArgument {
				t.Fatalf("invalid canonical manifest = %v, want invalid argument", applyErr)
			}
		})
	}
	_, err = remote.ApplyApplication(context.Background(), bytes.Repeat([]byte("x"), maxManifestBytes+1), "idempotency-key-too-large")
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized manifest = %v, want resource exhausted", err)
	}
	noncanonical := append([]byte("\n"), manifest...)
	_, err = remote.ApplyApplication(context.Background(), noncanonical, "idempotency-key-noncanonical")
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("noncanonical manifest = %v, want invalid argument", err)
	}
	both := &orchv1alpha1.ApplyApplicationRequest{
		ManifestToml: []byte("legacy"), CanonicalManifestJson: manifest, IdempotencyKey: "idempotency-key-both-fields",
	}
	_, err = unknownClient.ApplyApplication(context.Background(), connect.NewRequest(both))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "cannot both") {
		t.Fatalf("both manifest fields = %v, want invalid argument", err)
	}
	unknownApply := &orchv1alpha1.ApplyApplicationRequest{
		CanonicalManifestJson: manifest,
		IdempotencyKey:        "idempotency-key-unknown-protobuf",
	}
	unknownApply.ProtoReflect().SetUnknown([]byte{0x20, 0x01})
	_, err = unknownClient.ApplyApplication(context.Background(), connect.NewRequest(unknownApply))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown apply field = %v, want invalid argument", err)
	}
}

func TestControllerReconcilesStoredCanonicalStateWithoutPkl(t *testing.T) {
	_, dataDir, contextPath, _ := bootstrapController(t)
	manifest := newTestManifest("hello", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	})
	canonical := mustCanonicalConfig(t, manifest)
	digest, err := config.Digest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(context.Background(), filepath.Join(dataDir, clusterctl.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ApplyApplication(context.Background(), model.ApplyApplication{
		Name: manifest.Name, RevisionID: "revision-existing", Digest: digest,
		CanonicalManifest: canonical, IdempotencyKey: "idempotency-existing-state",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		_ = state.Close()
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", t.TempDir())
	server, err := Start(context.Background(), Options{DataDir: dataDir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	rewriteContextServer(t, contextPath, "https://"+server.Addr().String())
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.CloseIdleConnections)
	application, err := remote.GetApplication(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if application.GetDigest() != digest || application.GetCanonicalManifestJson() != string(canonical) {
		t.Fatalf("stored application changed without Pkl: %#v", application)
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
	manifest := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	}))
	firstApply, err := remote.ApplyApplication(context.Background(), manifest, "idempotency-key-one")
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
	replay, err := restarted.ApplyApplication(context.Background(), manifest, "idempotency-key-one")
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
