package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
)

func TestAgentCSRSigningAndIdentityRoundTrip(t *testing.T) {
	t.Parallel()
	const clusterID = "e6207444-6574-4b99-a2d2-3d6db1325c14"
	const nodeID = "node_0123456789abcdef0123456789abcdef"
	bundle, err := Generate(GenerateOptions{
		ClusterID: clusterID, ClusterName: "test", Server: "https://controller.example",
		Now: pkiTestTime, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	csr, key, wantHash, err := GenerateAgentCertificateRequest("node-1", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentCertificateRequestKey(csr, key); err != nil {
		t.Fatal(err)
	}
	if gotHash, err := AgentCSRPublicKeyHash(csr); err != nil || gotHash != wantHash {
		t.Fatalf("AgentCSRPublicKeyHash() = %q, %v; want %q", gotHash, err, wantHash)
	}
	signed, err := SignAgentCertificate(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, clusterID, nodeID, csr, pkiTestTime, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if signed.PublicKeyHash != wantHash || signed.Serial == "" || !signed.NotAfter.After(pkiTestTime) {
		t.Fatalf("signed certificate = %#v", signed)
	}
	if err := VerifyAgentIdentity(bundle.CACertificatePEM, KeyPair{CertificatePEM: signed.CertificatePEM, PrivateKeyPEM: key}, clusterID, nodeID, pkiTestTime); err != nil {
		t.Fatal(err)
	}
	certificate := mustCertificate(t, signed.CertificatePEM)
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != AgentIdentityURI(clusterID, nodeID).String() {
		t.Fatalf("agent URIs = %v", certificate.URIs)
	}
}

func TestAgentCSRAndIdentityRejectMismatches(t *testing.T) {
	t.Parallel()
	const clusterID = "e6207444-6574-4b99-a2d2-3d6db1325c14"
	const nodeID = "node_0123456789abcdef0123456789abcdef"
	bundle, err := Generate(GenerateOptions{
		ClusterID: clusterID, ClusterName: "test", Server: "https://controller.example",
		Now: pkiTestTime, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	csr, key, _, err := GenerateAgentCertificateRequest("node-1", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, _, err := GenerateAgentCertificateRequest("node-2", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentCertificateRequestKey(csr, otherKey); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched CSR key = %v", err)
	}

	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384CSR, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "wrong-curve"}}, p384Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AgentCSRPublicKeyHash(p384CSR); err == nil || !strings.Contains(err.Error(), "P-256") {
		t.Fatalf("P-384 CSR = %v", err)
	}
	tampered := append([]byte(nil), csr...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := AgentCSRPublicKeyHash(tampered); err == nil {
		t.Fatal("tampered CSR was accepted")
	}

	signed, err := SignAgentCertificate(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, clusterID, nodeID, csr, pkiTestTime, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := mustCertificate(t, signed.CertificatePEM)
	tests := []struct {
		name   string
		mutate func(*x509.Certificate)
	}{
		{name: "wrong node", mutate: func(cert *x509.Certificate) { cert.URIs = []*url.URL{AgentIdentityURI(clusterID, "node_other")} }},
		{name: "duplicate URI", mutate: func(cert *x509.Certificate) { cert.URIs = append(cert.URIs, cert.URIs[0]) }},
		{name: "wrong usage", mutate: func(cert *x509.Certificate) { cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} }},
		{name: "CA", mutate: func(cert *x509.Certificate) { cert.IsCA = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := *certificate
			copy.URIs = append([]*url.URL(nil), certificate.URIs...)
			test.mutate(&copy)
			if err := VerifyAgentPeerIdentity(&copy, clusterID, nodeID); err == nil {
				t.Fatalf("VerifyAgentPeerIdentity() accepted %s", test.name)
			}
		})
	}

	foreign, err := Generate(GenerateOptions{
		ClusterID: "00000000-0000-4000-8000-000000000000", ClusterName: "foreign",
		Server: "https://controller.example", Now: pkiTestTime, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentIdentity(foreign.CACertificatePEM, KeyPair{CertificatePEM: signed.CertificatePEM, PrivateKeyPEM: key}, clusterID, nodeID, pkiTestTime); err == nil {
		t.Fatal("foreign CA accepted the agent certificate")
	}
	if err := VerifyAgentIdentity(bundle.CACertificatePEM, KeyPair{CertificatePEM: signed.CertificatePEM, PrivateKeyPEM: key}, clusterID, nodeID, signed.NotAfter.AddDate(0, 0, 1)); err == nil {
		t.Fatal("expired agent certificate was accepted")
	}
}

func TestSignAgentIgnoresRequestedSANs(t *testing.T) {
	t.Parallel()
	const clusterID = "e6207444-6574-4b99-a2d2-3d6db1325c14"
	bundle, err := Generate(GenerateOptions{ClusterID: clusterID, ClusterName: "test", Server: "https://controller.example", Now: pkiTestTime, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "hostile"}, DNSNames: []string{"admin.example"},
		URIs: []*url.URL{IdentityURI(clusterID, RoleAdministrator)},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignAgentCertificate(bundle.CACertificatePEM, bundle.CAPrivateKeyPEM, clusterID, "node_safe", csr, pkiTestTime, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := mustCertificate(t, signed.CertificatePEM)
	if len(cert.DNSNames) != 0 || len(cert.URIs) != 1 || cert.URIs[0].String() != AgentIdentityURI(clusterID, "node_safe").String() {
		t.Fatalf("signed SANs = DNS %v, URI %v", cert.DNSNames, cert.URIs)
	}

	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgentCertificateRequestKey(csr, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})); err != nil {
		t.Fatal(err)
	}
}
