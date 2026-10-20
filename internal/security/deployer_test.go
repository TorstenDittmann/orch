package security

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestDeployerCertificateNamesOneApplication(t *testing.T) {
	now := time.Now()
	bundle, err := Generate(GenerateOptions{ClusterID: "cluster-one", ClusterName: "test", Server: "https://127.0.0.1", Now: now, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := GenerateDeployerCertificateRequest(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, notAfter, err := SignDeployerCertificate(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, "cluster-one", "hello", csr, now, rand.Reader)
	if err != nil || notAfter.Sub(now) != DeployerCertificateLifetime {
		t.Fatalf("SignDeployerCertificate() = %v, %v", notAfter, err)
	}
	block, _ := pem.Decode(certificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if application, err := DeployerApplication(certificate, "cluster-one"); err != nil || application != "hello" {
		t.Fatalf("DeployerApplication() = %q, %v", application, err)
	}
	if _, err := DeployerApplication(certificate, "cluster-two"); err == nil {
		t.Fatal("deployer certificate accepted for another cluster")
	}
	if err := VerifyPeerIdentity(certificate, "cluster-one", RoleAdministrator); err == nil {
		t.Fatal("deployer certificate accepted as administrator")
	}
	adminBlock, _ := pem.Decode(bundle.Administrator.CertificatePEM)
	admin, err := x509.ParseCertificate(adminBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeployerApplication(admin, "cluster-one"); err == nil {
		t.Fatal("administrator certificate accepted as deployer")
	}
	if _, _, err := SignDeployerCertificate(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, "cluster-one", "a/b", csr, now, rand.Reader); err == nil {
		t.Fatal("application with a slash was signed")
	}
}

func TestViewerCertificateIsReadOnly(t *testing.T) {
	now := time.Now()
	bundle, err := Generate(GenerateOptions{ClusterID: "cluster-one", ClusterName: "test", Server: "https://127.0.0.1", Now: now, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := GenerateDeployerCertificateRequest(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, _, err := SignApplicationCertificate(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, "cluster-one", "hello", true, csr, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if application, readOnly, err := ApplicationCredential(certificate, "cluster-one"); err != nil || application != "hello" || !readOnly {
		t.Fatalf("ApplicationCredential() = %q, %t, %v", application, readOnly, err)
	}
	if _, err := DeployerApplication(certificate, "cluster-one"); err == nil {
		t.Fatal("viewer certificate accepted as deployer")
	}
}

func TestShortLivedAdministratorCertificate(t *testing.T) {
	now := time.Now()
	bundle, err := Generate(GenerateOptions{ClusterID: "cluster-one", ClusterName: "test", Server: "https://127.0.0.1", Now: now, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	csr, _, err := GenerateDeployerCertificateRequest(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, notAfter, err := SignAdministratorCertificate(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, "cluster-one", csr, now, rand.Reader)
	if err != nil || notAfter.Sub(now) != DeployerCertificateLifetime {
		t.Fatalf("SignAdministratorCertificate() = %v, %v", notAfter, err)
	}
	block, _ := pem.Decode(certificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPeerIdentity(certificate, "cluster-one", RoleAdministrator); err != nil {
		t.Fatalf("administrator certificate rejected: %v", err)
	}
	if err := VerifyPeerIdentity(certificate, "cluster-two", RoleAdministrator); err == nil {
		t.Fatal("administrator certificate accepted for another cluster")
	}
	if _, _, err := ApplicationCredential(certificate, "cluster-one"); err == nil {
		t.Fatal("administrator certificate accepted as an application credential")
	}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	id, secret, hash, err := NewAccessTokenSecret(rand.Reader)
	if err != nil || hash != AccessTokenSecretHash(secret) {
		t.Fatal(err)
	}
	address := "https://203.0.113.10:9443#sha256:00ff"
	parsedAddress, credential, err := ParseAccessToken(EncodeAccessToken(address, id, secret))
	if err != nil || parsedAddress != address || credential != id+"."+secret {
		t.Fatalf("ParseAccessToken() = %q, %q, %v", parsedAddress, credential, err)
	}
	for _, value := range []string{"", "orch_", "orch_a.b.c", "token " + secret} {
		if _, _, err := ParseAccessToken(value); err == nil {
			t.Fatalf("ParseAccessToken(%q) succeeded", value)
		}
	}
}
