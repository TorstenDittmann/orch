package control

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/security"
)

func TestRenewAdministratorCertificateOnlyForContextCertificates(t *testing.T) {
	state, cluster := reconcilerStore(t)
	now := reconcilerTestTime.Add(300 * 24 * time.Hour)
	bundle, err := security.Generate(security.GenerateOptions{ClusterID: cluster.ID, ClusterName: cluster.Name,
		Server: "https://controller.example", Now: reconcilerTestTime, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	cluster.Endpoint = "https://controller.example"
	s := newService(state, cluster, bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, nil, nil, func() time.Time { return now })
	call := func(principal authenticatedPrincipal) (*connect.Response[orchv1alpha1.RenewAdministratorCertificateResponse], error) {
		ctx := context.WithValue(context.Background(), principalKey{}, principal)
		return s.RenewAdministratorCertificate(ctx, connect.NewRequest(&orchv1alpha1.RenewAdministratorCertificateRequest{}))
	}
	contextCertificate := &x509.Certificate{NotBefore: reconcilerTestTime, NotAfter: reconcilerTestTime.AddDate(1, 0, 0)}
	response, err := call(authenticatedPrincipal{certificate: contextCertificate})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Msg.GetExpiresAt().AsTime().Equal(now.AddDate(1, 0, 0)) {
		t.Fatalf("renewed certificate expires %s", response.Msg.GetExpiresAt().AsTime())
	}
	if err := security.VerifyIdentity(bundle.CACertificatePEM, security.KeyPair{CertificatePEM: response.Msg.GetCertificatePem(), PrivateKeyPEM: response.Msg.GetPrivateKeyPem()},
		cluster.ID, security.RoleAdministrator, cluster.Endpoint, now); err != nil {
		t.Fatalf("renewed identity: %v", err)
	}

	tokenCertificate := &x509.Certificate{NotBefore: now.Add(-time.Minute), NotAfter: now.Add(15 * time.Minute)}
	if _, err := call(authenticatedPrincipal{certificate: tokenCertificate}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("access token certificate renewal = %v", err)
	}
	if _, err := call(authenticatedPrincipal{local: true}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("socket renewal = %v", err)
	}
	if _, err := call(authenticatedPrincipal{certificate: contextCertificate, application: "hello"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("deployer renewal = %v", err)
	}
}
