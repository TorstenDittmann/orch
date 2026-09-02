package cluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
