package control

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"strings"
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

func TestAccessTokensGiveLimitedAccessToOneApplication(t *testing.T) {
	_, dataDir, contextPath, _ := bootstrapController(t)
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
	admin, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.CloseIdleConnections)
	ctx := context.Background()

	deployToken, err := admin.CreateAccessToken(ctx, "hello", "hello", false, time.Hour)
	if err != nil || deployToken.GetToken().GetReadOnly() || !strings.HasPrefix(deployToken.GetValue(), "orch_") {
		t.Fatalf("CreateAccessToken() = %+v, %v", deployToken, err)
	}
	viewToken, err := admin.CreateAccessToken(ctx, "hello-read-only", "hello", true, time.Hour)
	if err != nil || !viewToken.GetToken().GetReadOnly() {
		t.Fatalf("CreateAccessToken(read-only) = %+v, %v", viewToken, err)
	}
	if _, err := admin.CreateAccessToken(ctx, "hello", "hello", false, time.Hour); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate name = %v", err)
	}
	if _, err := admin.CreateAccessToken(ctx, "long", "hello", false, 400*24*time.Hour); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("too long a lifetime = %v", err)
	}
	listed, err := admin.ListAccessTokens(ctx)
	if err != nil || len(listed) != 2 || listed[0].GetName() != "hello" || listed[0].GetLastUsedAt() != nil {
		t.Fatalf("ListAccessTokens() = %+v, %v", listed, err)
	}

	exchange := orchv1alpha1connect.NewCredentialServiceClient(tlsHTTPClient(credentials.RootCAs, tls.Certificate{}), endpoint)
	connectWith := func(value string) (orchv1alpha1connect.ControlServiceClient, error) {
		_, credential, err := security.ParseAccessToken(value)
		if err != nil {
			t.Fatal(err)
		}
		csr, key, err := security.GenerateDeployerCertificateRequest(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		response, err := exchange.ExchangeAccessToken(ctx, connect.NewRequest(&orchv1alpha1.ExchangeAccessTokenRequest{Token: credential, CertificateRequestDer: csr}))
		if err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(response.Msg.GetCertificatePem(), key)
		if err != nil {
			t.Fatal(err)
		}
		return generatedClient(tlsHTTPClient(credentials.RootCAs, pair), endpoint), nil
	}
	if _, err := exchange.ExchangeAccessToken(ctx, connect.NewRequest(&orchv1alpha1.ExchangeAccessTokenRequest{
		Token: "0123456789abcdef." + strings.Repeat("A", 43), CertificateRequestDer: []byte{1},
	})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unknown token = %v", err)
	}

	deployer, err := connectWith(deployToken.GetValue())
	if err != nil {
		t.Fatal(err)
	}
	hello := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	}))
	if _, err := deployer.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: hello, IdempotencyKey: "token-hello-key-0001"})); err != nil {
		t.Fatalf("deploy with token = %v", err)
	}
	other := mustCanonicalConfig(t, newTestManifest("other", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"},
	}))
	if _, err := deployer.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: other, IdempotencyKey: "token-other-key-0001"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deploy another application = %v", err)
	}
	privileged := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"docker": {Image: "example/docker@sha256:1111111111111111111111111111111111111111111111111111111111111111", Privileged: true},
	}))
	if _, err := deployer.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: privileged, IdempotencyKey: "token-privileged-key-1"})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "privileged services (docker)") {
		t.Fatalf("deployer deploying a privileged service = %v", err)
	}
	capable := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{
		"web": {Image: "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111", Capabilities: []string{"CAP_SETUID"}},
	}))
	if _, err := deployer.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: capable, IdempotencyKey: "token-capabilities-key-1"})); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "Linux capabilities (web)") {
		t.Fatalf("deployer granting capabilities = %v", err)
	}
	adminRevision, err := admin.ApplyApplication(ctx, capable, "admin-capabilities-key-1")
	if err != nil {
		t.Fatalf("administrator granting capabilities = %v", err)
	}
	if _, err := deployer.RollbackApplication(ctx, connect.NewRequest(&orchv1alpha1.RollbackApplicationRequest{
		Application: "hello", RevisionId: adminRevision.GetRevisionId(), ExpectedGeneration: adminRevision.GetGeneration(), IdempotencyKey: "token-capability-rollback",
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer rolling back to capabilities = %v", err)
	}
	if _, err := deployer.CreateAccessToken(ctx, connect.NewRequest(&orchv1alpha1.CreateAccessTokenRequest{Name: "escalate", Application: "other"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer creating a token = %v", err)
	}

	viewer, err := connectWith(viewToken.GetValue())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.GetApplication(ctx, connect.NewRequest(&orchv1alpha1.GetApplicationRequest{Name: "hello"})); err != nil {
		t.Fatalf("viewer GetApplication = %v", err)
	}
	if _, err := viewer.ListDeployments(ctx, connect.NewRequest(&orchv1alpha1.ListDeploymentsRequest{Application: "hello", Limit: 5})); err != nil {
		t.Fatalf("viewer ListDeployments = %v", err)
	}
	if _, err := viewer.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: hello, IdempotencyKey: "viewer-hello-key-0001"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer deploy = %v", err)
	}
	if _, err := viewer.RollbackApplication(ctx, connect.NewRequest(&orchv1alpha1.RollbackApplicationRequest{Application: "hello"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer rollback = %v", err)
	}
	if _, err := viewer.GetApplication(ctx, connect.NewRequest(&orchv1alpha1.GetApplicationRequest{Name: "other"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer reading another application = %v", err)
	}

	if _, err := viewer.ListCronRuns(ctx, connect.NewRequest(&orchv1alpha1.ListCronRunsRequest{Application: "hello", Limit: 5})); err != nil {
		t.Fatalf("viewer ListCronRuns = %v", err)
	}
	if _, err := viewer.ListCronRuns(ctx, connect.NewRequest(&orchv1alpha1.ListCronRunsRequest{Application: "other", Limit: 5})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer cron runs of another application = %v", err)
	}
	tailLogs := func(application string) error {
		stream, err := viewer.TailLogs(ctx, connect.NewRequest(&orchv1alpha1.TailLogsRequest{Application: application, Service: "web", Replica: 1, TailLines: 10}))
		if err != nil {
			return err
		}
		defer stream.Close()
		for stream.Receive() {
		}
		return stream.Err()
	}
	// No agent runs the replica in this test, so reading its logs gets past
	// authorization and then fails for lack of a node.
	if err := tailLogs("hello"); err == nil || connect.CodeOf(err) == connect.CodePermissionDenied {
		t.Fatalf("viewer logs of own application = %v", err)
	}
	if err := tailLogs("other"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer logs of another application = %v", err)
	}
	if _, err := viewer.ListSecrets(ctx, connect.NewRequest(&orchv1alpha1.ListSecretsRequest{Application: "hello"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer ListSecrets = %v", err)
	}

	adminService := generatedClient(tlsHTTPClient(credentials.RootCAs, credentials.Certificate), endpoint)
	if _, err := adminService.CreateAccessToken(ctx, connect.NewRequest(&orchv1alpha1.CreateAccessTokenRequest{Name: "mixed", Application: "hello", Administrator: true})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("administrator token limited to an application = %v", err)
	}
	if _, err := adminService.CreateAccessToken(ctx, connect.NewRequest(&orchv1alpha1.CreateAccessTokenRequest{Name: "mixed", ReadOnly: true, Administrator: true})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("read-only administrator token = %v", err)
	}
	adminToken, err := admin.CreateAdministratorAccessToken(ctx, "panel", time.Hour)
	if err != nil || !adminToken.GetToken().GetAdministrator() || adminToken.GetToken().GetApplication() != "" {
		t.Fatalf("CreateAdministratorAccessToken() = %+v, %v", adminToken, err)
	}
	if _, err := deployer.CreateAccessToken(ctx, connect.NewRequest(&orchv1alpha1.CreateAccessTokenRequest{Name: "escalate", Administrator: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer creating an administrator token = %v", err)
	}
	administrator, err := connectWith(adminToken.GetValue())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := administrator.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: other, IdempotencyKey: "admin-other-key-0001"})); err != nil {
		t.Fatalf("administrator token deploying any application = %v", err)
	}
	if _, err := administrator.ListNodes(ctx, connect.NewRequest(&orchv1alpha1.ListNodesRequest{})); err != nil {
		t.Fatalf("administrator token ListNodes = %v", err)
	}
	if _, err := administrator.ListSecrets(ctx, connect.NewRequest(&orchv1alpha1.ListSecretsRequest{Application: "hello"})); err != nil {
		t.Fatalf("administrator token ListSecrets = %v", err)
	}

	listed, err = admin.ListAccessTokens(ctx)
	if err != nil || listed[0].GetLastUsedAt() == nil {
		t.Fatalf("last use was not recorded: %+v, %v", listed, err)
	}
	if revoked, err := admin.DeleteAccessToken(ctx, "hello"); err != nil || !revoked {
		t.Fatalf("DeleteAccessToken() = %v, %v", revoked, err)
	}
	if _, err := connectWith(deployToken.GetValue()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("exchange after revoke = %v", err)
	}
}
