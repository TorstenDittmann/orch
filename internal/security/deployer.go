package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// RoleDeployer identifies short-lived certificates that may deploy exactly one
// application. They are issued from trusted CI identity tokens, never stored.
const RoleDeployer = "deployer"

// DeployerCertificateLifetime bounds how long a CI job may use one credential.
const DeployerCertificateLifetime = 15 * time.Minute

// DeployerIdentityURI binds a deployer certificate to one application.
func DeployerIdentityURI(clusterID, application string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "orch", Path: "/cluster/" + clusterID + "/" + RoleDeployer + "/" + application}
}

// GenerateDeployerCertificateRequest creates a P-256 key and CSR for a
// deployer credential. The key stays in the CI job's memory.
func GenerateDeployerCertificateRequest(random io.Reader) (csrDER, privateKeyPEM []byte, err error) {
	if random == nil {
		return nil, nil, fmt.Errorf("random source is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return nil, nil, fmt.Errorf("generate deployer key: %w", err)
	}
	csrDER, err = x509.CreateCertificateRequest(random, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "orch-deployer", Organization: []string{"orch"}},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create deployer certificate request: %w", err)
	}
	privateKeyPEM, err = encodePrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode deployer key: %w", err)
	}
	return csrDER, privateKeyPEM, nil
}

// SignDeployerCertificate signs only the validated CSR public key for one
// application. Requested SANs and extensions are deliberately ignored.
func SignDeployerCertificate(caCertificatePEM, caPrivateKeyPEM []byte, clusterID, application string, csrDER []byte, now time.Time, random io.Reader) ([]byte, time.Time, error) {
	if clusterID == "" || application == "" || strings.Contains(application, "/") || now.IsZero() || random == nil {
		return nil, time.Time{}, fmt.Errorf("cluster, application, current time, and random source are required")
	}
	if _, err := AgentCSRPublicKeyHash(csrDER); err != nil {
		return nil, time.Time{}, err
	}
	request, _ := x509.ParseCertificateRequest(csrDER)
	ca, err := parseCertificate(caCertificatePEM)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse CA certificate: %w", err)
	}
	caKey, err := parseECDSAPrivateKey(caPrivateKeyPEM)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse CA private key: %w", err)
	}
	serial, err := serialNumber(random)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("generate deployer serial: %w", err)
	}
	keyID, err := subjectKeyID(request.PublicKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("derive deployer key id: %w", err)
	}
	notAfter := now.UTC().Add(DeployerCertificateLifetime)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	if !notAfter.After(now.UTC()) {
		return nil, time.Time{}, fmt.Errorf("CA certificate expires too soon to sign a deployer identity")
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "orch-deployer-" + application, Organization: []string{"orch"}},
		NotBefore:             now.UTC().Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:                  []*url.URL{DeployerIdentityURI(clusterID, application)},
		SubjectKeyId:          keyID,
		AuthorityKeyId:        append([]byte(nil), ca.SubjectKeyId...),
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(random, template, ca, request.PublicKey, caKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("sign deployer certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), notAfter, nil
}

// DeployerApplication validates an already chain-verified TLS peer as a
// deployer for this cluster and returns the one application it may change.
func DeployerApplication(certificate *x509.Certificate, clusterID string) (string, error) {
	if certificate == nil {
		return "", fmt.Errorf("deployer certificate is missing")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if certificate.IsCA || !ok || publicKey.Curve != elliptic.P256() ||
		!sameUsages(certificate.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) {
		return "", fmt.Errorf("deployer certificate has unexpected constraints or extended key usages")
	}
	if len(certificate.URIs) != 1 {
		return "", fmt.Errorf("deployer certificate does not contain exactly one identity")
	}
	uri := certificate.URIs[0]
	prefix := "/cluster/" + clusterID + "/" + RoleDeployer + "/"
	if uri.Scheme != "spiffe" || uri.Host != "orch" || !strings.HasPrefix(uri.Path, prefix) {
		return "", fmt.Errorf("deployer certificate does not contain an identity for this cluster")
	}
	application := strings.TrimPrefix(uri.Path, prefix)
	if application == "" || strings.Contains(application, "/") || uri.String() != DeployerIdentityURI(clusterID, application).String() {
		return "", fmt.Errorf("deployer certificate contains an invalid application")
	}
	return application, nil
}
