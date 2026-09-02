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
)

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
	expectedUsages := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if role == RoleController {
		expectedUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	} else if role != RoleAdministrator {
		return fmt.Errorf("unknown identity role %q", role)
	}
	if !sameUsages(certificate.ExtKeyUsage, expectedUsages) {
		return fmt.Errorf("%s certificate has unexpected extended key usages", role)
	}
	expectedURI := IdentityURI(clusterID, role).String()
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != expectedURI {
		return fmt.Errorf("%s certificate does not contain identity %s", role, expectedURI)
	}
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
