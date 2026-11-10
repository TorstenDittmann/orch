package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
)

// AdministratorRenewalWindow is how long before expiry the CLI asks the
// controller for a new administrator certificate.
const AdministratorRenewalWindow = 30 * 24 * time.Hour

// ReplaceAdministratorIdentity installs a renewed administrator certificate
// and key for an existing context. The new pair is written to new files next
// to the current ones and the context is then switched to them with one
// atomic rename, so an interruption never leaves a key that does not match
// its certificate. The previous files are removed afterwards.
func ReplaceAdministratorIdentity(contextPath string, certificatePEM, privateKeyPEM []byte, now time.Time) error {
	absolute, err := filepath.Abs(contextPath)
	if err != nil {
		return fmt.Errorf("resolve administrator context: %w", err)
	}
	current, err := LoadAdministratorCredentials(absolute, now)
	if err != nil {
		return err
	}
	if err := security.VerifyIdentity(current.CACertificatePEM, security.KeyPair{
		CertificatePEM: certificatePEM, PrivateKeyPEM: privateKeyPEM,
	}, current.Context.Cluster.ID, security.RoleAdministrator, current.Context.Server, now.UTC()); err != nil {
		return fmt.Errorf("validate renewed administrator identity: %w", err)
	}
	contextDirectory := filepath.Dir(absolute)
	oldCertificate, _ := resolveContextReference(absolute, current.Context.Credentials.ClientCertificate)
	oldKey, _ := resolveContextReference(absolute, current.Context.Credentials.ClientKey)
	directory := filepath.Dir(oldCertificate)
	stamp := now.UTC().Format("20060102T150405")
	newCertificate := filepath.Join(directory, "admin-"+stamp+".pem")
	newKey := filepath.Join(directory, "admin-key-"+stamp+".pem")
	written := []string{}
	cleanup := func() {
		for _, path := range written {
			_ = os.Remove(path)
		}
	}
	for path, contents := range map[string][]byte{newKey: privateKeyPEM, newCertificate: certificatePEM} {
		if err := writePrivateFile(path, contents); err != nil {
			cleanup()
			return fmt.Errorf("write renewed administrator identity: %w", err)
		}
		written = append(written, path)
	}
	if err := syncDirectory(directory); err != nil {
		cleanup()
		return err
	}
	updated := current.Context
	updated.Credentials.ClientCertificate = relativeContextPath(contextDirectory, newCertificate)
	updated.Credentials.ClientKey = relativeContextPath(contextDirectory, newKey)
	contents, err := encodeContext(updated)
	if err != nil {
		cleanup()
		return err
	}
	staged, err := stageFile(contextDirectory, ".context-*.json", contents)
	if err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(staged, absolute); err != nil {
		_ = os.Remove(staged)
		cleanup()
		return fmt.Errorf("switch administrator context: %w", err)
	}
	if err := syncDirectory(contextDirectory); err != nil {
		return err
	}
	if _, err := LoadAdministratorCredentials(absolute, now); err != nil {
		return fmt.Errorf("renewed administrator context: %w", err)
	}
	for _, path := range []string{oldCertificate, oldKey} {
		if path != newCertificate && path != newKey {
			_ = os.Remove(path)
		}
	}
	return nil
}
