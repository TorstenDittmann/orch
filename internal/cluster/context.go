package cluster

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
)

const ContextVersion = "v1alpha1"

type ContextCluster struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ContextCredentials struct {
	CertificateAuthority string `json:"certificate_authority"`
	ClientCertificate    string `json:"client_certificate"`
	ClientKey            string `json:"client_key"`
}

// LocalContext is the portable, non-secret description used by an
// administrator client. Credential paths are relative to the context file.
type LocalContext struct {
	Version     string             `json:"version"`
	Cluster     ContextCluster     `json:"cluster"`
	Server      string             `json:"server"`
	Credentials ContextCredentials `json:"credentials"`
}

// AdministratorCredentials is a verified context and its TLS material.
type AdministratorCredentials struct {
	Context          LocalContext
	Certificate      tls.Certificate
	RootCAs          *x509.CertPool
	ServerName       string
	CACertificatePEM []byte
}

// LoadContext reads one strict JSON context and rejects insecure permissions.
func LoadContext(contextPath string) (LocalContext, error) {
	contents, err := readPrivateFile(contextPath)
	if err != nil {
		return LocalContext{}, fmt.Errorf("read administrator context: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var local LocalContext
	if err := decoder.Decode(&local); err != nil {
		return LocalContext{}, fmt.Errorf("decode administrator context: %w", err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return LocalContext{}, fmt.Errorf("decode administrator context: %w", err)
	}
	if err := validateContext(local); err != nil {
		return LocalContext{}, err
	}
	return local, nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("context contains more than one JSON value")
}

func validateContext(local LocalContext) error {
	if local.Version != ContextVersion {
		return fmt.Errorf("unsupported context version %q", local.Version)
	}
	if local.Cluster.ID == "" || local.Cluster.Name == "" || local.Server == "" {
		return fmt.Errorf("context cluster id, name, and server are required")
	}
	normalizedServer, err := normalizeServer(local.Server)
	if err != nil || normalizedServer != local.Server {
		return fmt.Errorf("context server must be a normalized HTTPS URL")
	}
	credentials := []struct {
		name string
		path string
	}{
		{name: "certificate_authority", path: local.Credentials.CertificateAuthority},
		{name: "client_certificate", path: local.Credentials.ClientCertificate},
		{name: "client_key", path: local.Credentials.ClientKey},
	}
	for _, credential := range credentials {
		if _, err := resolveContextReference("context.json", credential.path); err != nil {
			return fmt.Errorf("invalid context credential %s: %w", credential.name, err)
		}
	}
	return nil
}

// LoadAdministratorCredentials resolves and validates all context TLS files.
func LoadAdministratorCredentials(contextPath string, now time.Time) (AdministratorCredentials, error) {
	absolute, err := filepath.Abs(contextPath)
	if err != nil {
		return AdministratorCredentials{}, fmt.Errorf("resolve administrator context: %w", err)
	}
	local, err := LoadContext(absolute)
	if err != nil {
		return AdministratorCredentials{}, err
	}
	caPath, _ := resolveContextReference(absolute, local.Credentials.CertificateAuthority)
	certificatePath, _ := resolveContextReference(absolute, local.Credentials.ClientCertificate)
	keyPath, _ := resolveContextReference(absolute, local.Credentials.ClientKey)
	for _, directory := range privateCredentialDirectories(filepath.Dir(absolute), caPath, certificatePath, keyPath) {
		if err := requirePrivateDirectory(directory); err != nil {
			return AdministratorCredentials{}, fmt.Errorf("validate credential directory: %w", err)
		}
	}
	caPEM, err := readPrivateFile(caPath)
	if err != nil {
		return AdministratorCredentials{}, fmt.Errorf("read administrator CA certificate: %w", err)
	}
	certificatePEM, err := readPrivateFile(certificatePath)
	if err != nil {
		return AdministratorCredentials{}, fmt.Errorf("read administrator certificate: %w", err)
	}
	keyPEM, err := readPrivateFile(keyPath)
	if err != nil {
		return AdministratorCredentials{}, fmt.Errorf("read administrator private key: %w", err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	if err := security.VerifyIdentity(caPEM, security.KeyPair{
		CertificatePEM: certificatePEM, PrivateKeyPEM: keyPEM,
	}, local.Cluster.ID, security.RoleAdministrator, local.Server, now.UTC()); err != nil {
		return AdministratorCredentials{}, fmt.Errorf("validate administrator identity: %w", err)
	}
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return AdministratorCredentials{}, fmt.Errorf("load administrator key pair: %w", err)
	}
	if len(pair.Certificate) != 1 {
		return AdministratorCredentials{}, fmt.Errorf("administrator certificate file must contain exactly one certificate")
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return AdministratorCredentials{}, fmt.Errorf("parse administrator certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return AdministratorCredentials{}, fmt.Errorf("load administrator CA certificate")
	}
	serverURL, _ := url.Parse(local.Server)
	return AdministratorCredentials{
		Context: local, Certificate: pair, RootCAs: roots, ServerName: serverURL.Hostname(),
		CACertificatePEM: append([]byte(nil), caPEM...),
	}, nil
}

func privateCredentialDirectories(contextDirectory string, paths ...string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		for directory := filepath.Dir(path); directory != contextDirectory; directory = filepath.Dir(directory) {
			if _, exists := seen[directory]; !exists {
				seen[directory] = struct{}{}
				result = append(result, directory)
			}
			if filepath.Dir(directory) == directory {
				break
			}
		}
	}
	return result
}

func resolveContextReference(contextPath, reference string) (string, error) {
	if strings.TrimSpace(reference) == "" {
		return "", fmt.Errorf("path is required")
	}
	native := filepath.FromSlash(reference)
	if filepath.IsAbs(native) || filepath.Clean(native) != native || native == "." || native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q must be a clean relative path", reference)
	}
	return filepath.Join(filepath.Dir(contextPath), native), nil
}

func readPrivateFile(path string) ([]byte, error) {
	if err := requirePrivateFile(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func requirePrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s permissions %04o allow group or world access", path, info.Mode().Perm())
	}
	return nil
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
