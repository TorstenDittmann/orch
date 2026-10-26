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

// RoleViewer identifies short-lived certificates that may only read one
// application. They are issued for read-only access tokens.
const RoleViewer = "viewer"

// DeployerCertificateLifetime bounds how long a CI job may use one credential.
const DeployerCertificateLifetime = 15 * time.Minute

// DeployerIdentityURI binds a deployer certificate to one application.
func DeployerIdentityURI(clusterID, application string) *url.URL {
	return applicationIdentityURI(clusterID, application, false)
}

func applicationIdentityURI(clusterID, application string, readOnly bool) *url.URL {
	role := RoleDeployer
	if readOnly {
		role = RoleViewer
	}
	return &url.URL{Scheme: "spiffe", Host: "orch", Path: "/cluster/" + clusterID + "/" + role + "/" + application}
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
func SignDeployerCertificate(caCertificatePEM, caPrivateKeyPEM []byte, clusterID, application string, csrDER []byte, now time.Time, random io.Reader, domains ...string) ([]byte, time.Time, error) {
	return SignApplicationCertificate(caCertificatePEM, caPrivateKeyPEM, clusterID, application, false, csrDER, now, random, domains...)
}

// SignApplicationCertificate signs a deployer certificate, or with readOnly a
// viewer certificate that may only read the application. Domains, when given,
// are the only domains the certificate may route; see ApplicationDomains.
func SignApplicationCertificate(caCertificatePEM, caPrivateKeyPEM []byte, clusterID, application string, readOnly bool, csrDER []byte, now time.Time, random io.Reader, domains ...string) ([]byte, time.Time, error) {
	if application == "" || strings.Contains(application, "/") {
		return nil, time.Time{}, fmt.Errorf("cluster, application, current time, and random source are required")
	}
	return signShortLivedCertificate(caCertificatePEM, caPrivateKeyPEM, clusterID, "orch-deployer-"+application,
		applicationIdentityURI(clusterID, application, readOnly), csrDER, now, random, domains...)
}

// SignAdministratorCertificate signs a short-lived administrator certificate
// for an administrator access token. It carries the same identity as the
// administrator certificate in a context file, so it may do everything an
// administrator can, but only until it expires.
func SignAdministratorCertificate(caCertificatePEM, caPrivateKeyPEM []byte, clusterID string, csrDER []byte, now time.Time, random io.Reader) ([]byte, time.Time, error) {
	return signShortLivedCertificate(caCertificatePEM, caPrivateKeyPEM, clusterID, "orch-"+RoleAdministrator,
		IdentityURI(clusterID, RoleAdministrator), csrDER, now, random)
}

// signShortLivedCertificate signs only the validated CSR public key with one
// identity URI. Requested SANs and extensions are deliberately ignored.
func signShortLivedCertificate(caCertificatePEM, caPrivateKeyPEM []byte, clusterID, commonName string, identity *url.URL, csrDER []byte, now time.Time, random io.Reader, domains ...string) ([]byte, time.Time, error) {
	if clusterID == "" || now.IsZero() || random == nil {
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
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"orch"}},
		NotBefore:             now.UTC().Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:                  []*url.URL{identity},
		DNSNames:              append([]string(nil), domains...),
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
	application, readOnly, err := ApplicationCredential(certificate, clusterID)
	if err == nil && readOnly {
		return "", fmt.Errorf("viewer certificate may not deploy")
	}
	return application, err
}

// ApplicationCredential validates an already chain-verified TLS peer as a
// deployer or viewer for this cluster and returns its one application.
func ApplicationCredential(certificate *x509.Certificate, clusterID string) (string, bool, error) {
	if certificate == nil {
		return "", false, fmt.Errorf("deployer certificate is missing")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if certificate.IsCA || !ok || publicKey.Curve != elliptic.P256() ||
		!sameUsages(certificate.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) {
		return "", false, fmt.Errorf("deployer certificate has unexpected constraints or extended key usages")
	}
	if len(certificate.URIs) != 1 {
		return "", false, fmt.Errorf("deployer certificate does not contain exactly one identity")
	}
	uri := certificate.URIs[0]
	for _, readOnly := range []bool{false, true} {
		role := RoleDeployer
		if readOnly {
			role = RoleViewer
		}
		prefix := "/cluster/" + clusterID + "/" + role + "/"
		if uri.Scheme != "spiffe" || uri.Host != "orch" || !strings.HasPrefix(uri.Path, prefix) {
			continue
		}
		application := strings.TrimPrefix(uri.Path, prefix)
		if application == "" || strings.Contains(application, "/") || uri.String() != applicationIdentityURI(clusterID, application, readOnly).String() {
			return "", false, fmt.Errorf("deployer certificate contains an invalid application")
		}
		return application, readOnly, nil
	}
	return "", false, fmt.Errorf("deployer certificate does not contain an identity for this cluster")
}

// ApplicationDomains returns the domains an application certificate may
// route, each with its subdomains, or nil when it may route any domain. The
// controller writes them as DNS names when it signs the certificate.
func ApplicationDomains(certificate *x509.Certificate) []string {
	if certificate == nil || len(certificate.DNSNames) == 0 {
		return nil
	}
	return append([]string(nil), certificate.DNSNames...)
}

// DomainAllowed reports whether domain is one of allowed or a subdomain of
// one. An empty allowed list allows every domain.
func DomainAllowed(domain string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, suffix := range allowed {
		if domain == suffix || strings.HasSuffix(domain, "."+suffix) {
			return true
		}
	}
	return false
}
