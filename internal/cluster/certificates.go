package cluster

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

type RenewCertificatesOptions struct {
	DataDir  string
	AdminDir string
	Now      func() time.Time
	Random   io.Reader
}

type RenewCertificatesResult struct {
	Cluster                      model.Cluster
	DataDir                      string
	ContextPath                  string
	ControllerNotAfter           time.Time
	AdministratorNotAfter        time.Time
	CertificateAuthorityNotAfter time.Time
}

// RenewCertificates reissues the controller server identity and a new
// administrator identity from the existing cluster CA. Only the CA has to be
// valid, so this also recovers a controller whose leaf has already expired.
// The administrator directory is published first and never overwritten; the
// controller leaf is then replaced in place and takes effect on restart.
func RenewCertificates(ctx context.Context, options RenewCertificatesOptions) (RenewCertificatesResult, error) {
	var result RenewCertificatesResult
	if strings.TrimSpace(options.DataDir) == "" || strings.TrimSpace(options.AdminDir) == "" {
		return result, fmt.Errorf("%w: data directory and administrator directory are required", ErrInvalidOptions)
	}
	dataDir, err := filepath.Abs(options.DataDir)
	if err != nil {
		return result, fmt.Errorf("%w: resolve data directory: %v", ErrInvalidOptions, err)
	}
	adminDir, err := filepath.Abs(options.AdminDir)
	if err != nil {
		return result, fmt.Errorf("%w: resolve administrator directory: %v", ErrInvalidOptions, err)
	}
	dataDir = filepath.Clean(dataDir)
	adminDir = filepath.Clean(adminDir)
	if filepath.Dir(adminDir) == adminDir || pathsOverlap(dataDir, adminDir) {
		return result, fmt.Errorf("%w: administrator directory must be a new non-root path outside the data directory", ErrInvalidOptions)
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	current := now().UTC()

	if err := requirePrivateDirectory(dataDir); err != nil {
		return result, fmt.Errorf("validate data directory: %w", err)
	}
	databasePath := filepath.Join(dataDir, DatabaseFilename)
	if err := requirePrivateFile(databasePath); err != nil {
		return result, fmt.Errorf("validate controller database: %w", err)
	}
	state, err := store.OpenInitialized(ctx, databasePath)
	if err != nil {
		return result, fmt.Errorf("open cluster state: %w", err)
	}
	stored, err := state.Cluster(ctx)
	closeErr := state.Close()
	if err != nil {
		return result, fmt.Errorf("read cluster state: %w", err)
	}
	if closeErr != nil {
		return result, closeErr
	}

	pki := filepath.Join(dataDir, controllerIdentityDirectory)
	if err := requirePrivateDirectory(pki); err != nil {
		return result, fmt.Errorf("validate controller identity directory: %w", err)
	}
	caPEM, err := readPrivateFile(filepath.Join(pki, "ca.pem"))
	if err != nil {
		return result, fmt.Errorf("read controller identity ca.pem: %w", err)
	}
	caKeyPEM, err := readPrivateFile(filepath.Join(pki, "ca-key.pem"))
	if err != nil {
		return result, fmt.Errorf("read controller identity ca-key.pem: %w", err)
	}
	if err := security.VerifyCertificateAuthority(caPEM, caKeyPEM, stored.Name, current); err != nil {
		return result, fmt.Errorf("validate cluster CA: %w", err)
	}
	caNotAfter, err := security.CertificateNotAfter(caPEM)
	if err != nil {
		return result, fmt.Errorf("parse CA certificate: %w", err)
	}
	issue := func(role, server string) (security.KeyPair, error) {
		pair, err := security.IssueIdentity(caPEM, caKeyPEM, security.IssueOptions{
			ClusterID: stored.ID, Role: role, Server: server, Now: current, Random: random,
		})
		if err != nil {
			return security.KeyPair{}, fmt.Errorf("issue %s identity: %w", role, err)
		}
		if err := security.VerifyIdentity(caPEM, pair, stored.ID, role, stored.Endpoint, current); err != nil {
			return security.KeyPair{}, fmt.Errorf("validate %s identity: %w", role, err)
		}
		return pair, nil
	}
	controller, err := issue(security.RoleController, stored.Endpoint)
	if err != nil {
		return result, err
	}
	administrator, err := issue(security.RoleAdministrator, "")
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	contextPath, err := publishAdministratorDirectory(adminDir, stored, caPEM, administrator)
	if err != nil {
		return result, err
	}
	if err := replaceControllerLeaf(pki, controller); err != nil {
		return result, fmt.Errorf("replace controller identity: %w; the new administrator directory remains valid; rerun with a different --admin-dir to retry", err)
	}
	if _, err := LoadControllerCredentials(dataDir, stored, current); err != nil {
		return result, err
	}
	controllerNotAfter, _ := security.CertificateNotAfter(controller.CertificatePEM)
	administratorNotAfter, _ := security.CertificateNotAfter(administrator.CertificatePEM)
	return RenewCertificatesResult{
		Cluster: stored, DataDir: dataDir, ContextPath: contextPath,
		ControllerNotAfter: controllerNotAfter, AdministratorNotAfter: administratorNotAfter,
		CertificateAuthorityNotAfter: caNotAfter,
	}, nil
}

// publishAdministratorDirectory writes the same portable layout that
// bootstrap and cluster install produce into a newly reserved directory.
func publishAdministratorDirectory(adminDir string, stored model.Cluster, caPEM []byte, administrator security.KeyPair) (contextPath string, finalErr error) {
	parent := filepath.Dir(adminDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create administrator directory parent: %w", err)
	}
	if err := requireAbsent(adminDir, "administrator directory"); err != nil {
		return "", err
	}
	if err := os.Mkdir(adminDir, 0o700); err != nil {
		return "", fmt.Errorf("reserve administrator directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(adminDir)
		}
	}()
	identityRoot := filepath.Join(adminDir, administratorRootDirectory)
	identityDirectory := filepath.Join(identityRoot, stored.ID)
	for _, directory := range []string{identityRoot, identityDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return "", fmt.Errorf("create administrator identity directory: %w", err)
		}
	}
	for name, contents := range map[string][]byte{
		"ca.pem":        caPEM,
		"admin.pem":     administrator.CertificatePEM,
		"admin-key.pem": administrator.PrivateKeyPEM,
	} {
		if err := writePrivateFile(filepath.Join(identityDirectory, name), contents); err != nil {
			return "", fmt.Errorf("write administrator identity %s: %w", name, err)
		}
	}
	for _, directory := range []string{identityDirectory, identityRoot} {
		if err := syncDirectory(directory); err != nil {
			return "", err
		}
	}
	contextPath = filepath.Join(adminDir, "context.json")
	contents, err := encodeContext(LocalContext{
		Version: ContextVersion,
		Cluster: ContextCluster{ID: stored.ID, Name: stored.Name},
		Server:  stored.Endpoint,
		Credentials: ContextCredentials{
			CertificateAuthority: relativeContextPath(adminDir, filepath.Join(identityDirectory, "ca.pem")),
			ClientCertificate:    relativeContextPath(adminDir, filepath.Join(identityDirectory, "admin.pem")),
			ClientKey:            relativeContextPath(adminDir, filepath.Join(identityDirectory, "admin-key.pem")),
		},
	})
	if err != nil {
		return "", err
	}
	if err := writePrivateFile(contextPath, contents); err != nil {
		return "", fmt.Errorf("write administrator context: %w", err)
	}
	if err := syncDirectory(adminDir); err != nil {
		return "", err
	}
	if err := syncDirectory(parent); err != nil {
		return "", err
	}
	published = true
	return contextPath, nil
}

// replaceControllerLeaf atomically renames each staged file over the live one,
// preserving the identity directory's owner so a service account can still
// read it. The key and certificate are separate renames; an interruption
// between them is repaired by rerunning the renewal with a new --admin-dir,
// which needs only the CA.
func replaceControllerLeaf(pki string, controller security.KeyPair) error {
	files := []struct {
		name     string
		contents []byte
	}{
		{name: "controller-key.pem", contents: controller.PrivateKeyPEM},
		{name: "controller.pem", contents: controller.CertificatePEM},
	}
	staged := make([]string, 0, len(files))
	defer func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}()
	for _, file := range files {
		path, err := stageFile(pki, ".orch-renew-*", file.contents)
		if err != nil {
			return err
		}
		staged = append(staged, path)
		if err := matchOwner(path, pki); err != nil {
			return err
		}
	}
	for index, file := range files {
		if err := os.Rename(staged[index], filepath.Join(pki, file.name)); err != nil {
			return err
		}
	}
	return syncDirectory(pki)
}
