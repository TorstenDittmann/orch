package security

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"time"
)

const (
	RoleController    = "controller"
	RoleAdministrator = "administrator"
	RoleAgent         = "agent"
)

const agentCertificateLifetime = 365 * 24 * time.Hour

// KeyPair contains one PEM-encoded certificate and its PKCS#8 private key.
type KeyPair struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
}

// Bundle is all identity material created for a single-node cluster.
type Bundle struct {
	CACertificatePEM []byte
	CAPrivateKeyPEM  []byte
	Controller       KeyPair
	Administrator    KeyPair
}

type GenerateOptions struct {
	ClusterID   string
	ClusterName string
	Server      string
	Now         time.Time
	Random      io.Reader
}

// SignedAgentCertificate is public enrollment material derived from a CSR.
type SignedAgentCertificate struct {
	CertificatePEM []byte
	PublicKeyHash  string
	Serial         string
	NotAfter       time.Time
}

// Generate creates a private cluster CA plus purpose-limited controller and
// administrator identities. Callers are responsible for persisting the
// returned bytes with restrictive permissions.
func Generate(options GenerateOptions) (Bundle, error) {
	if options.ClusterID == "" || options.ClusterName == "" {
		return Bundle{}, fmt.Errorf("cluster id and name are required")
	}
	if options.Random == nil {
		return Bundle{}, fmt.Errorf("random source is required")
	}
	if options.Now.IsZero() {
		return Bundle{}, fmt.Errorf("current time is required")
	}
	server, err := url.Parse(options.Server)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" {
		return Bundle{}, fmt.Errorf("server must be an HTTPS URL")
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), options.Random)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate CA key: %w", err)
	}
	caSerial, err := serialNumber(options.Random)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate CA serial: %w", err)
	}
	now := options.Now.UTC()
	caKeyID, err := subjectKeyID(&caKey.PublicKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("derive CA key id: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: options.ClusterName + " orch CA", Organization: []string{"orch"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          caKeyID,
	}
	caDER, err := x509.CreateCertificate(options.Random, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("create CA certificate: %w", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Bundle{}, fmt.Errorf("parse generated CA certificate: %w", err)
	}

	controller, err := generateIdentity(options.Random, caCertificate, caKey, identityOptions{
		clusterID: options.ClusterID,
		role:      RoleController,
		now:       now,
		server:    server,
		usages:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return Bundle{}, err
	}
	administrator, err := generateIdentity(options.Random, caCertificate, caKey, identityOptions{
		clusterID: options.ClusterID,
		role:      RoleAdministrator,
		now:       now,
		usages:    []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return Bundle{}, err
	}
	caKeyPEM, err := encodePrivateKey(caKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("encode CA key: %w", err)
	}
	return Bundle{
		CACertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CAPrivateKeyPEM:  caKeyPEM,
		Controller:       controller,
		Administrator:    administrator,
	}, nil
}

type identityOptions struct {
	clusterID string
	role      string
	now       time.Time
	server    *url.URL
	usages    []x509.ExtKeyUsage
}

func generateIdentity(random io.Reader, ca *x509.Certificate, caKey *ecdsa.PrivateKey, options identityOptions) (KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generate %s key: %w", options.role, err)
	}
	serial, err := serialNumber(random)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generate %s serial: %w", options.role, err)
	}
	identityURI := IdentityURI(options.clusterID, options.role)
	keyID, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		return KeyPair{}, fmt.Errorf("derive %s key id: %w", options.role, err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "orch-" + options.role,
			Organization: []string{"orch"},
		},
		NotBefore:             options.now.Add(-5 * time.Minute),
		NotAfter:              options.now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           options.usages,
		URIs:                  []*url.URL{identityURI},
		SubjectKeyId:          keyID,
		BasicConstraintsValid: true,
	}
	if options.role == RoleController {
		hostname := options.server.Hostname()
		if address := net.ParseIP(hostname); address != nil {
			template.IPAddresses = []net.IP{address}
		} else {
			template.DNSNames = []string{hostname}
		}
	}
	certificateDER, err := x509.CreateCertificate(random, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return KeyPair{}, fmt.Errorf("create %s certificate: %w", options.role, err)
	}
	keyPEM, err := encodePrivateKey(key)
	if err != nil {
		return KeyPair{}, fmt.Errorf("encode %s key: %w", options.role, err)
	}
	return KeyPair{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		PrivateKeyPEM:  keyPEM,
	}, nil
}

func serialNumber(random io.Reader) (*big.Int, error) {
	value := make([]byte, 20)
	if _, err := io.ReadFull(random, value); err != nil {
		return nil, err
	}
	value[0] &= 0x7f
	serial := new(big.Int).SetBytes(value)
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func encodePrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func subjectKeyID(publicKey crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(der)
	return digest[:20], nil
}

// IdentityURI is the stable URI SAN used to bind a certificate to a cluster
// and one narrowly defined role.
func IdentityURI(clusterID, role string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "orch", Path: "/cluster/" + clusterID + "/" + role}
}

// AgentIdentityURI binds an agent certificate to exactly one cluster node.
func AgentIdentityURI(clusterID, nodeID string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: "orch", Path: "/cluster/" + clusterID + "/agent/" + nodeID}
}

// GenerateAgentCertificateRequest creates a P-256 key and signed CSR. The key
// is returned only to the local agent enrollment workflow.
func GenerateAgentCertificateRequest(nodeName string, random io.Reader) (csrDER, privateKeyPEM []byte, publicKeyHash string, err error) {
	if nodeName == "" || random == nil {
		return nil, nil, "", fmt.Errorf("node name and random source are required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate agent key: %w", err)
	}
	csrDER, err = x509.CreateCertificateRequest(random, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "orch-agent-" + nodeName, Organization: []string{"orch"}},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create agent certificate request: %w", err)
	}
	privateKeyPEM, err = encodePrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("encode agent key: %w", err)
	}
	publicKeyHash, err = hashPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, "", fmt.Errorf("hash agent public key: %w", err)
	}
	return csrDER, privateKeyPEM, publicKeyHash, nil
}

// AgentCSRPublicKeyHash validates a signed P-256 CSR and returns the canonical
// SHA-256 fingerprint of its public key.
func AgentCSRPublicKeyHash(csrDER []byte) (string, error) {
	if len(csrDER) == 0 {
		return "", fmt.Errorf("agent certificate request is required")
	}
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return "", fmt.Errorf("parse agent certificate request: %w", err)
	}
	if err := request.CheckSignature(); err != nil {
		return "", fmt.Errorf("verify agent certificate request signature: %w", err)
	}
	publicKey, ok := request.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return "", fmt.Errorf("agent certificate request must use an ECDSA P-256 key")
	}
	return hashPublicKey(publicKey)
}

// VerifyAgentCertificateRequestKey ensures a staged private key belongs to a
// validated enrollment CSR.
func VerifyAgentCertificateRequestKey(csrDER, privateKeyPEM []byte) error {
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return fmt.Errorf("parse agent certificate request: %w", err)
	}
	if _, err := AgentCSRPublicKeyHash(csrDER); err != nil {
		return err
	}
	key, err := parseECDSAPrivateKey(privateKeyPEM)
	if err != nil {
		return fmt.Errorf("parse agent private key: %w", err)
	}
	requestPublic, err := x509.MarshalPKIXPublicKey(request.PublicKey)
	if err != nil {
		return err
	}
	keyPublic, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return err
	}
	if !bytes.Equal(requestPublic, keyPublic) {
		return fmt.Errorf("agent private key does not match certificate request")
	}
	return nil
}

// SignAgentCertificate signs only the validated CSR public key. Requested SANs
// and extensions are deliberately ignored.
func SignAgentCertificate(caCertificatePEM, caPrivateKeyPEM []byte, clusterID, nodeID string, csrDER []byte, now time.Time, random io.Reader) (SignedAgentCertificate, error) {
	if clusterID == "" || nodeID == "" || now.IsZero() || random == nil {
		return SignedAgentCertificate{}, fmt.Errorf("cluster, node, current time, and random source are required")
	}
	publicKeyHash, err := AgentCSRPublicKeyHash(csrDER)
	if err != nil {
		return SignedAgentCertificate{}, err
	}
	request, _ := x509.ParseCertificateRequest(csrDER)
	ca, err := parseCertificate(caCertificatePEM)
	if err != nil {
		return SignedAgentCertificate{}, fmt.Errorf("parse CA certificate: %w", err)
	}
	caKey, err := parseECDSAPrivateKey(caPrivateKeyPEM)
	if err != nil {
		return SignedAgentCertificate{}, fmt.Errorf("parse CA private key: %w", err)
	}
	if err := verifyPrivateKey(ca, caPrivateKeyPEM); err != nil {
		return SignedAgentCertificate{}, fmt.Errorf("verify CA private key: %w", err)
	}
	serial, err := serialNumber(random)
	if err != nil {
		return SignedAgentCertificate{}, fmt.Errorf("generate agent serial: %w", err)
	}
	keyID, err := subjectKeyID(request.PublicKey)
	if err != nil {
		return SignedAgentCertificate{}, fmt.Errorf("derive agent key id: %w", err)
	}
	notAfter := now.UTC().Add(agentCertificateLifetime)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	if !notAfter.After(now.UTC()) {
		return SignedAgentCertificate{}, fmt.Errorf("CA certificate expires too soon to sign an agent identity")
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "orch-agent-" + nodeID, Organization: []string{"orch"}},
		NotBefore:             now.UTC().Add(-5 * time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:                  []*url.URL{AgentIdentityURI(clusterID, nodeID)},
		SubjectKeyId:          keyID,
		AuthorityKeyId:        append([]byte(nil), ca.SubjectKeyId...),
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(random, template, ca, request.PublicKey, caKey)
	if err != nil {
		return SignedAgentCertificate{}, fmt.Errorf("sign agent certificate: %w", err)
	}
	return SignedAgentCertificate{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		PublicKeyHash:  publicKeyHash,
		Serial:         serial.String(),
		NotAfter:       notAfter,
	}, nil
}

// VerifyAgentIdentity validates an agent certificate, private key, exact node
// URI, and chain for local startup.
func VerifyAgentIdentity(caPEM []byte, pair KeyPair, clusterID, nodeID string, now time.Time) error {
	ca, err := parseCertificate(caPEM)
	if err != nil {
		return fmt.Errorf("parse CA certificate: %w", err)
	}
	certificate, err := parseCertificate(pair.CertificatePEM)
	if err != nil {
		return fmt.Errorf("parse agent certificate: %w", err)
	}
	if err := VerifyAgentPeerIdentity(certificate, clusterID, nodeID); err != nil {
		return err
	}
	if err := verifyPrivateKey(certificate, pair.PrivateKeyPEM); err != nil {
		return fmt.Errorf("verify agent private key: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := certificate.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: now.UTC(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("verify agent certificate: %w", err)
	}
	return nil
}

// VerifyAgentPeerIdentity validates an already chain-verified TLS peer.
func VerifyAgentPeerIdentity(certificate *x509.Certificate, clusterID, nodeID string) error {
	if certificate == nil {
		return fmt.Errorf("agent certificate is missing")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if certificate.IsCA || !ok || publicKey.Curve != elliptic.P256() ||
		certificate.KeyUsage != x509.KeyUsageDigitalSignature ||
		!sameUsages(certificate.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) {
		return fmt.Errorf("agent certificate has unexpected constraints or extended key usages")
	}
	expectedURI := AgentIdentityURI(clusterID, nodeID).String()
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != expectedURI {
		return fmt.Errorf("agent certificate does not contain identity %s", expectedURI)
	}
	return nil
}

// CertificatePublicKeyHash returns the same fingerprint used by enrollment.
func CertificatePublicKeyHash(certificate *x509.Certificate) (string, error) {
	if certificate == nil {
		return "", fmt.Errorf("certificate is missing")
	}
	return hashPublicKey(certificate.PublicKey)
}

// VerifyCertificateAuthority checks the CA certificate, its private key, and
// its binding to the named cluster.
func VerifyCertificateAuthority(certificatePEM, privateKeyPEM []byte, clusterName string, now time.Time) error {
	certificate, err := parseCertificate(certificatePEM)
	if err != nil {
		return fmt.Errorf("parse CA certificate: %w", err)
	}
	if !certificate.IsCA || certificate.Subject.CommonName != clusterName+" orch CA" {
		return fmt.Errorf("CA certificate does not belong to cluster %q", clusterName)
	}
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return fmt.Errorf("CA certificate is not valid at the current time")
	}
	if err := certificate.CheckSignatureFrom(certificate); err != nil {
		return fmt.Errorf("verify self-signed CA certificate: %w", err)
	}
	if err := verifyPrivateKey(certificate, privateKeyPEM); err != nil {
		return fmt.Errorf("verify CA private key: %w", err)
	}
	return nil
}

// VerifyIdentity checks a leaf certificate, key, role URI, exact extended key
// usages, CA chain, and (for a controller) the server hostname.
func VerifyIdentity(caPEM []byte, pair KeyPair, clusterID, role, server string, now time.Time) error {
	ca, err := parseCertificate(caPEM)
	if err != nil {
		return fmt.Errorf("parse CA certificate: %w", err)
	}
	certificate, err := parseCertificate(pair.CertificatePEM)
	if err != nil {
		return fmt.Errorf("parse %s certificate: %w", role, err)
	}
	if err := VerifyPeerIdentity(certificate, clusterID, role); err != nil {
		return err
	}
	expectedUsages, _ := identityUsages(role)
	if err := verifyPrivateKey(certificate, pair.PrivateKeyPEM); err != nil {
		return fmt.Errorf("verify %s private key: %w", role, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, usage := range expectedUsages {
		options := x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}
		if role == RoleController && usage == x509.ExtKeyUsageServerAuth {
			parsed, parseErr := url.Parse(server)
			if parseErr != nil || parsed.Hostname() == "" {
				return fmt.Errorf("parse controller server URL")
			}
			options.DNSName = parsed.Hostname()
		}
		if _, err := certificate.Verify(options); err != nil {
			return fmt.Errorf("verify %s certificate: %w", role, err)
		}
	}
	return nil
}

// VerifyPeerIdentity enforces the exact role and cluster URI carried by an
// already chain-verified TLS peer certificate.
func VerifyPeerIdentity(certificate *x509.Certificate, clusterID, role string) error {
	if certificate == nil {
		return fmt.Errorf("%s certificate is missing", role)
	}
	expectedUsages, err := identityUsages(role)
	if err != nil {
		return err
	}
	if certificate.IsCA {
		return fmt.Errorf("%s identity must not be a CA certificate", role)
	}
	if !sameUsages(certificate.ExtKeyUsage, expectedUsages) {
		return fmt.Errorf("%s certificate has unexpected extended key usages", role)
	}
	expectedURI := IdentityURI(clusterID, role).String()
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != expectedURI {
		return fmt.Errorf("%s certificate does not contain identity %s", role, expectedURI)
	}
	return nil
}

func identityUsages(role string) ([]x509.ExtKeyUsage, error) {
	switch role {
	case RoleController:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, nil
	case RoleAdministrator:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil
	default:
		return nil, fmt.Errorf("unknown identity role %q", role)
	}
}

func sameUsages(actual, expected []x509.ExtKeyUsage) bool {
	if len(actual) != len(expected) {
		return false
	}
	remaining := make(map[x509.ExtKeyUsage]int, len(expected))
	for _, usage := range expected {
		remaining[usage]++
	}
	for _, usage := range actual {
		if remaining[usage] == 0 {
			return false
		}
		remaining[usage]--
	}
	return true
}

func parseCertificate(contents []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("expected exactly one PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func verifyPrivateKey(certificate *x509.Certificate, contents []byte) error {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("expected exactly one PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return fmt.Errorf("private key cannot sign")
	}
	certificatePublic, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		return err
	}
	keyPublic, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return err
	}
	if !bytes.Equal(certificatePublic, keyPublic) {
		return fmt.Errorf("private key does not match certificate")
	}
	return nil
}

func parseECDSAPrivateKey(contents []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("expected exactly one PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("private key must be ECDSA P-256")
	}
	return key, nil
}

func hashPublicKey(publicKey crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(der)
	return fmt.Sprintf("%x", digest[:]), nil
}
