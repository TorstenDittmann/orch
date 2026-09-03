package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"path/filepath"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/security"
)

// ControllerCredentials is the verified TLS identity used by the server.
type ControllerCredentials struct {
	Certificate      tls.Certificate
	ClientCAs        *x509.CertPool
	CACertificatePEM []byte
	CAPrivateKeyPEM  []byte
}

// LoadControllerCredentials validates the protected on-disk identity against
// durable cluster state before returning any TLS material.
func LoadControllerCredentials(dataDir string, stored model.Cluster, now time.Time) (ControllerCredentials, error) {
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return ControllerCredentials{}, fmt.Errorf("resolve controller data directory: %w", err)
	}
	if err := requirePrivateDirectory(absolute); err != nil {
		return ControllerCredentials{}, fmt.Errorf("validate controller data directory: %w", err)
	}
	if err := requirePrivateFile(filepath.Join(absolute, DatabaseFilename)); err != nil {
		return ControllerCredentials{}, fmt.Errorf("validate controller database: %w", err)
	}
	identityDirectory := filepath.Join(absolute, controllerIdentityDirectory)
	if err := requirePrivateDirectory(identityDirectory); err != nil {
		return ControllerCredentials{}, fmt.Errorf("validate controller identity directory: %w", err)
	}
	read := func(name string) ([]byte, error) {
		contents, err := readPrivateFile(filepath.Join(identityDirectory, name))
		if err != nil {
			return nil, fmt.Errorf("read controller identity %s: %w", name, err)
		}
		return contents, nil
	}
	caPEM, err := read("ca.pem")
	if err != nil {
		return ControllerCredentials{}, err
	}
	caKeyPEM, err := read("ca-key.pem")
	if err != nil {
		return ControllerCredentials{}, err
	}
	certificatePEM, err := read("controller.pem")
	if err != nil {
		return ControllerCredentials{}, err
	}
	keyPEM, err := read("controller-key.pem")
	if err != nil {
		return ControllerCredentials{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	if err := security.VerifyCertificateAuthority(caPEM, caKeyPEM, stored.Name, now.UTC()); err != nil {
		return ControllerCredentials{}, fmt.Errorf("validate cluster CA: %w", err)
	}
	if err := security.VerifyIdentity(caPEM, security.KeyPair{
		CertificatePEM: certificatePEM, PrivateKeyPEM: keyPEM,
	}, stored.ID, security.RoleController, stored.Endpoint, now.UTC()); err != nil {
		return ControllerCredentials{}, fmt.Errorf("validate controller identity: %w", err)
	}
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return ControllerCredentials{}, fmt.Errorf("load controller key pair: %w", err)
	}
	if len(pair.Certificate) != 1 {
		return ControllerCredentials{}, fmt.Errorf("controller certificate file must contain exactly one certificate")
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return ControllerCredentials{}, fmt.Errorf("parse controller certificate: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return ControllerCredentials{}, fmt.Errorf("load controller CA certificate")
	}
	return ControllerCredentials{
		Certificate: pair, ClientCAs: clientCAs,
		CACertificatePEM: append([]byte(nil), caPEM...),
		CAPrivateKeyPEM:  append([]byte(nil), caKeyPEM...),
	}, nil
}
