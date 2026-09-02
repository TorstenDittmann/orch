package security

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"io"
	"testing"
	"time"
)

var pkiTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

func TestGenerateCreatesPurposeLimitedClusterPKI(t *testing.T) {
	t.Parallel()
	bundle, err := Generate(GenerateOptions{
		ClusterID: "e6207444-6574-4b99-a2d2-3d6db1325c14", ClusterName: "test",
		Server: "https://controller.example:9443", Now: pkiTestTime, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCertificateAuthority(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, "test", pkiTestTime); err != nil {
		t.Fatal(err)
	}
	if err := VerifyIdentity(bundle.CACertificatePEM, bundle.Controller, "e6207444-6574-4b99-a2d2-3d6db1325c14", RoleController, "https://controller.example:9443", pkiTestTime); err != nil {
		t.Fatal(err)
	}
	if err := VerifyIdentity(bundle.CACertificatePEM, bundle.Administrator, "e6207444-6574-4b99-a2d2-3d6db1325c14", RoleAdministrator, "https://controller.example:9443", pkiTestTime); err != nil {
		t.Fatal(err)
	}

	ca := mustCertificate(t, bundle.CACertificatePEM)
	controller := mustCertificate(t, bundle.Controller.CertificatePEM)
	administrator := mustCertificate(t, bundle.Administrator.CertificatePEM)
	seen := map[string]bool{}
	for name, certificate := range map[string]*x509.Certificate{"ca": ca, "controller": controller, "administrator": administrator} {
		if certificate.SerialNumber.Sign() <= 0 {
			t.Errorf("%s serial is not positive", name)
		}
		if seen[certificate.SerialNumber.String()] {
			t.Errorf("%s serial is duplicated", name)
		}
		seen[certificate.SerialNumber.String()] = true
	}
	if got := administrator.ExtKeyUsage; !sameUsages(got, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) {
		t.Fatalf("administrator usages = %v", got)
	}
	if got := controller.ExtKeyUsage; !sameUsages(got, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}) {
		t.Fatalf("controller usages = %v", got)
	}
	if len(ca.SubjectKeyId) == 0 || !bytes.Equal(controller.AuthorityKeyId, ca.SubjectKeyId) || !bytes.Equal(administrator.AuthorityKeyId, ca.SubjectKeyId) {
		t.Fatal("leaf certificates are not bound to the CA key id")
	}
	if len(administrator.URIs) != 1 || administrator.URIs[0].String() != IdentityURI("e6207444-6574-4b99-a2d2-3d6db1325c14", RoleAdministrator).String() {
		t.Fatalf("administrator URIs = %v", administrator.URIs)
	}
	if err := VerifyIdentity(bundle.CACertificatePEM, bundle.Administrator, "e6207444-6574-4b99-a2d2-3d6db1325c14", RoleController, "https://controller.example:9443", pkiTestTime); err == nil {
		t.Fatal("administrator identity was accepted as a controller")
	}
}

func TestGenerateSupportsIPControllerEndpoint(t *testing.T) {
	t.Parallel()
	bundle, err := Generate(GenerateOptions{
		ClusterID: "e6207444-6574-4b99-a2d2-3d6db1325c14", ClusterName: "test",
		Server: "https://192.0.2.10:9443", Now: pkiTestTime, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyIdentity(bundle.CACertificatePEM, bundle.Controller, "e6207444-6574-4b99-a2d2-3d6db1325c14", RoleController, "https://192.0.2.10:9443", pkiTestTime); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratePropagatesRandomFailure(t *testing.T) {
	t.Parallel()
	_, err := Generate(GenerateOptions{
		ClusterID: "e6207444-6574-4b99-a2d2-3d6db1325c14", ClusterName: "test",
		Server: "https://controller.example", Now: pkiTestTime, Random: errorReader{},
	})
	if err == nil || !errors.Is(err, errNoRandomness) {
		t.Fatalf("Generate() = %v, want random source error", err)
	}
}

func TestVerifyIdentityRejectsWrongPrivateKey(t *testing.T) {
	t.Parallel()
	bundle, err := Generate(GenerateOptions{
		ClusterID: "e6207444-6574-4b99-a2d2-3d6db1325c14", ClusterName: "test",
		Server: "https://controller.example", Now: pkiTestTime, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair := bundle.Administrator
	pair.PrivateKeyPEM = bundle.Controller.PrivateKeyPEM
	if err := VerifyIdentity(bundle.CACertificatePEM, pair, "e6207444-6574-4b99-a2d2-3d6db1325c14", RoleAdministrator, "https://controller.example", pkiTestTime); err == nil {
		t.Fatal("VerifyIdentity() accepted a mismatched key")
	}
}

func mustCertificate(t *testing.T, contents []byte) *x509.Certificate {
	t.Helper()
	certificate, err := parseCertificate(contents)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

var errNoRandomness = errors.New("no randomness")

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errNoRandomness
}

var _ io.Reader = errorReader{}
