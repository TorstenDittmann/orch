package agent

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/security"
)

const (
	ConfigVersion        = 1
	ConfigFilename       = "agent.json"
	DatabaseFilename     = "agent.db"
	SecretCacheKeyFile   = "secret-cache.key"
	EnrollmentFilename   = ".enrollment.json"
	IdentityDirectory    = "pki"
	LogsDirectory        = "logs"
	VolumesDirectory     = "volumes"
	CACertificateFile    = "ca.pem"
	AgentCertificateFile = "agent.pem"
	AgentPrivateKeyFile  = "agent-key.pem"
)

type Config struct {
	Version   int    `json:"version"`
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	ClusterID string `json:"cluster_id"`
	Server    string `json:"server"`
}

type Credentials struct {
	Config      Config
	Certificate tls.Certificate
	RootCAs     *x509.CertPool
	ServerName  string
}

// Load validates a complete protected agent identity before it can be used.
func Load(dataDir string, now time.Time) (Credentials, error) {
	absolute, err := normalizeDataDir(dataDir)
	if err != nil {
		return Credentials{}, err
	}
	if err := requirePrivateDirectory(absolute); err != nil {
		return Credentials{}, fmt.Errorf("validate agent data directory: %w", err)
	}
	for _, directory := range []string{IdentityDirectory, LogsDirectory, VolumesDirectory} {
		if err := requirePrivateDirectory(filepath.Join(absolute, directory)); err != nil {
			return Credentials{}, fmt.Errorf("validate agent %s directory: %w", directory, err)
		}
	}
	configBytes, err := readPrivateFile(filepath.Join(absolute, ConfigFilename))
	if err != nil {
		return Credentials{}, fmt.Errorf("read agent config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(configBytes))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Credentials{}, fmt.Errorf("decode agent config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return Credentials{}, fmt.Errorf("decode agent config: multiple JSON values")
		}
		return Credentials{}, fmt.Errorf("decode agent config trailing data: %w", err)
	}
	if config.Version != ConfigVersion || config.NodeID == "" || config.NodeName == "" || config.ClusterID == "" {
		return Credentials{}, fmt.Errorf("agent config is incomplete or has an unsupported version")
	}
	server, err := url.Parse(config.Server)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || server.User != nil {
		return Credentials{}, fmt.Errorf("agent controller endpoint must be an HTTPS URL without user information")
	}
	identityDir := filepath.Join(absolute, IdentityDirectory)
	caPEM, err := readPrivateFile(filepath.Join(identityDir, CACertificateFile))
	if err != nil {
		return Credentials{}, fmt.Errorf("read agent CA certificate: %w", err)
	}
	certificatePEM, err := readPrivateFile(filepath.Join(identityDir, AgentCertificateFile))
	if err != nil {
		return Credentials{}, fmt.Errorf("read agent certificate: %w", err)
	}
	keyPEM, err := readPrivateFile(filepath.Join(identityDir, AgentPrivateKeyFile))
	if err != nil {
		return Credentials{}, fmt.Errorf("read agent private key: %w", err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	if err := security.VerifyAgentIdentity(caPEM, security.KeyPair{
		CertificatePEM: certificatePEM, PrivateKeyPEM: keyPEM,
	}, config.ClusterID, config.NodeID, now.UTC()); err != nil {
		return Credentials{}, fmt.Errorf("validate agent identity: %w", err)
	}
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return Credentials{}, fmt.Errorf("load agent key pair: %w", err)
	}
	if len(pair.Certificate) != 1 {
		return Credentials{}, fmt.Errorf("agent certificate file must contain exactly one certificate")
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Credentials{}, fmt.Errorf("parse agent certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return Credentials{}, fmt.Errorf("load agent CA certificate")
	}
	return Credentials{Config: config, Certificate: pair, RootCAs: roots, ServerName: server.Hostname()}, nil
}

func normalizeDataDir(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("agent data directory is required")
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve agent data directory: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if filepath.Dir(absolute) == absolute {
		return "", fmt.Errorf("agent data directory cannot be a filesystem root")
	}
	return absolute, nil
}

func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s permissions %04o allow group or world access", path, info.Mode().Perm())
	}
	return nil
}

func readPrivateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s permissions %04o allow group or world access", path, info.Mode().Perm())
	}
	return os.ReadFile(path)
}
