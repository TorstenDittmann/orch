package security

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ErrPinnedCAMismatch reports that a controller served a different CA than
// the pinned fingerprint.
var ErrPinnedCAMismatch = errors.New("controller CA does not match the pinned fingerprint; check the controller address")

// FetchPinnedCA connects to a controller, accepts the cluster CA it serves
// only if its SHA-256 fingerprint matches, verifies the controller against
// it, and returns the CA and the controller's cluster ID.
func FetchPinnedCA(ctx context.Context, endpoint string, fingerprint []byte) ([]byte, string, error) {
	server, err := url.Parse(endpoint)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" {
		return nil, "", errors.New("controller endpoint must be an HTTPS origin")
	}
	if len(fingerprint) != sha256.Size {
		return nil, "", errors.New("CA fingerprint must be a SHA-256 digest")
	}
	port := server.Port()
	if port == "" {
		port = "443"
	}
	var caPEM []byte
	var clusterID string
	config := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: server.Hostname(),
		// The chain is verified below against the pinned CA.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("controller presented no certificate")
			}
			var ca *x509.Certificate
			for _, candidate := range state.PeerCertificates[1:] {
				digest := sha256.Sum256(candidate.Raw)
				if subtle.ConstantTimeCompare(digest[:], fingerprint) == 1 {
					ca = candidate
					break
				}
			}
			if ca == nil || !ca.IsCA {
				return ErrPinnedCAMismatch
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca)
			leaf := state.PeerCertificates[0]
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: server.Hostname(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return fmt.Errorf("verify controller certificate: %w", err)
			}
			id, err := controllerClusterID(leaf)
			if err != nil {
				return err
			}
			if err := VerifyPeerIdentity(leaf, id, RoleController); err != nil {
				return fmt.Errorf("validate controller identity: %w", err)
			}
			caPEM, clusterID = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}), id
			return nil
		},
	}
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(server.Hostname(), port))
	if err != nil {
		return nil, "", fmt.Errorf("reach controller %s: %w", endpoint, err)
	}
	_ = connection.Close()
	if caPEM == nil {
		return nil, "", errors.New("controller CA was not verified")
	}
	return caPEM, clusterID, nil
}

func controllerClusterID(certificate *x509.Certificate) (string, error) {
	if len(certificate.URIs) != 1 {
		return "", errors.New("controller certificate does not contain exactly one identity")
	}
	uri := certificate.URIs[0]
	path := strings.TrimPrefix(uri.Path, "/cluster/")
	id, role, ok := strings.Cut(path, "/")
	if uri.Scheme != "spiffe" || uri.Host != "orch" || !strings.HasPrefix(uri.Path, "/cluster/") || !ok || id == "" || role != RoleController {
		return "", errors.New("controller certificate does not contain a controller identity")
	}
	return id, nil
}
