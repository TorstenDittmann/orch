package cluster

import (
	"context"
	"errors"
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

type RecoveryOptions struct {
	Backup    string
	PKIDir    string
	SecretKey string
	DataDir   string
	Now       func() time.Time
}

type RecoveryResult struct {
	store.RecoverySummary
	Ready   bool     `json:"ready"`
	Missing []string `json:"missing"`
	DataDir string   `json:"data_dir,omitempty"`
	// ControllerCertificateExpired marks recovery material whose CA is valid
	// but whose controller leaf has expired; renew it before starting.
	ControllerCertificateExpired bool `json:"controller_certificate_expired,omitempty"`
}

// InspectBackup works exclusively on a private temporary copy, including when
// checking migration compatibility. Missing optional material is reported.
func InspectBackup(ctx context.Context, options RecoveryOptions) (RecoveryResult, error) {
	stage, err := os.MkdirTemp("", ".orch-inspect-*")
	if err != nil {
		return RecoveryResult{}, err
	}
	defer os.RemoveAll(stage)
	return prepareRecovery(ctx, options, stage)
}

// Restore publishes a verified controller state into an exclusively reserved
// directory. control.db is linked last so an interrupted publication cannot
// expose a startable controller without its required key material.
func Restore(ctx context.Context, options RecoveryOptions) (result RecoveryResult, finalErr error) {
	if strings.TrimSpace(options.DataDir) == "" {
		return result, fmt.Errorf("%w: --data-dir is required", ErrInvalidOptions)
	}
	destination, err := filepath.Abs(options.DataDir)
	if err != nil {
		return result, err
	}
	if err := requireAbsent(destination, "restore data directory"); err != nil {
		return result, err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return result, err
	}
	if err := requireDirectory(parent); err != nil {
		return result, fmt.Errorf("restore parent directory: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".orch-restore-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	result, err = prepareRecovery(ctx, options, stage)
	if err != nil {
		return result, err
	}
	if !result.Ready {
		return result, fmt.Errorf("recovery material is missing: %s", strings.Join(result.Missing, ", "))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return result, fmt.Errorf("reserve restore data directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(destination)
		}
	}()
	pkiFiles := map[string][]byte{}
	for _, name := range controllerPKIFiles {
		pkiFiles[name] = nil
	}
	if err := publishDirectory(filepath.Join(stage, controllerIdentityDirectory), filepath.Join(destination, controllerIdentityDirectory), pkiFiles); err != nil {
		return result, err
	}
	if exists, err := pathExists(filepath.Join(stage, SecretKeyFilename)); err != nil {
		return result, err
	} else if exists {
		if err := publishFile(filepath.Join(stage, SecretKeyFilename), filepath.Join(destination, SecretKeyFilename)); err != nil {
			return result, err
		}
	}
	if err := syncDirectory(destination); err != nil {
		return result, err
	}
	if err := publishFile(filepath.Join(stage, DatabaseFilename), filepath.Join(destination, DatabaseFilename)); err != nil {
		return result, err
	}
	if err := syncDirectory(parent); err != nil {
		return result, err
	}
	published = true
	result.DataDir = destination
	return result, nil
}

var controllerPKIFiles = []string{"ca.pem", "ca-key.pem", "controller.pem", "controller-key.pem"}

func prepareRecovery(ctx context.Context, options RecoveryOptions, stage string) (result RecoveryResult, finalErr error) {
	result.Missing = []string{}
	if strings.TrimSpace(options.Backup) == "" {
		return result, fmt.Errorf("%w: --backup is required", ErrInvalidOptions)
	}
	source, err := filepath.Abs(options.Backup)
	if err != nil {
		return result, err
	}
	// A raw copy of a live WAL database can omit committed transactions. Only
	// standalone snapshots produced by cluster backup are recovery inputs.
	if err := requireStandaloneSnapshot(source); err != nil {
		return result, err
	}
	database := filepath.Join(stage, DatabaseFilename)
	if err := copyRecoveryFile(ctx, source, database, 0); err != nil {
		return result, fmt.Errorf("copy backup: %w", err)
	}
	if err := requireStandaloneSnapshot(source); err != nil {
		return result, err
	}
	state, summary, err := store.OpenRecoveryCopy(ctx, database)
	if err != nil {
		return result, fmt.Errorf("validate backup: %w", err)
	}
	defer func() {
		if err := state.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
	}()
	result.RecoverySummary = summary
	if normalized, err := normalizeServer(summary.Cluster.Endpoint); err != nil || normalized != summary.Cluster.Endpoint || !clusterName.MatchString(summary.Cluster.Name) {
		return result, fmt.Errorf("backup has invalid cluster identity metadata")
	}
	now := time.Now()
	if options.Now != nil {
		now = options.Now()
	}
	if options.PKIDir == "" {
		result.Missing = append(result.Missing, "--pki-dir")
	} else {
		if err := requirePrivateDirectory(options.PKIDir); err != nil {
			return result, fmt.Errorf("validate PKI directory: %w", err)
		}
		pki := filepath.Join(stage, controllerIdentityDirectory)
		if err := os.Mkdir(pki, 0o700); err != nil {
			return result, err
		}
		for _, name := range controllerPKIFiles {
			if err := copyRecoveryFile(ctx, filepath.Join(options.PKIDir, name), filepath.Join(pki, name), 1<<20); err != nil {
				return result, fmt.Errorf("copy controller identity %s: %w", name, err)
			}
		}
		if _, err := LoadControllerCredentials(stage, summary.Cluster, now); err != nil {
			expired, expiredErr := expiredControllerLeaf(stage, summary.Cluster, now)
			if expiredErr != nil || !expired {
				return result, err
			}
			result.ControllerCertificateExpired = true
		}
		if err := syncDirectory(pki); err != nil {
			return result, err
		}
	}
	if options.SecretKey == "" {
		if result.SecretKeyRequired {
			result.Missing = append(result.Missing, "--secret-key")
		}
	} else {
		key := filepath.Join(stage, SecretKeyFilename)
		if err := copyRecoveryFile(ctx, options.SecretKey, key, security.SecretKeySize); err != nil {
			return result, fmt.Errorf("copy secret key: %w", err)
		}
		box, err := security.LoadSecretBox(key, nil)
		if err != nil {
			return result, err
		}
		if err := state.VerifyRecoverySecrets(ctx, box); err != nil {
			return result, err
		}
	}
	if err := state.Close(); err != nil {
		return result, err
	}
	// Close checkpoints the migrated copy; sync its complete standalone image.
	file, err := os.Open(database)
	if err != nil {
		return result, err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	if err := syncDirectory(stage); err != nil {
		return result, err
	}
	result.Ready = len(result.Missing) == 0
	return result, nil
}

// expiredControllerLeaf reports whether the only problem with staged controller
// material is an expired leaf: the CA must be valid now and the leaf must have
// been a valid identity for this cluster just before it expired.
func expiredControllerLeaf(dataDir string, stored model.Cluster, now time.Time) (bool, error) {
	pki := filepath.Join(dataDir, controllerIdentityDirectory)
	caPEM, err := readPrivateFile(filepath.Join(pki, "ca.pem"))
	if err != nil {
		return false, err
	}
	caKeyPEM, err := readPrivateFile(filepath.Join(pki, "ca-key.pem"))
	if err != nil {
		return false, err
	}
	certificatePEM, err := readPrivateFile(filepath.Join(pki, "controller.pem"))
	if err != nil {
		return false, err
	}
	notAfter, err := security.CertificateNotAfter(certificatePEM)
	if err != nil || !now.After(notAfter) {
		return false, err
	}
	if err := security.VerifyCertificateAuthority(caPEM, caKeyPEM, stored.Name, now.UTC()); err != nil {
		return false, err
	}
	if _, err := LoadControllerCredentials(dataDir, stored, notAfter.Add(-time.Minute)); err != nil {
		return false, err
	}
	return true, nil
}

func requireStandaloneSnapshot(path string) error {
	if err := requirePrivateFile(path); err != nil {
		return fmt.Errorf("validate backup file: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("backup has SQLite sidecar %s; use a standalone cluster backup", suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func copyRecoveryFile(ctx context.Context, source, destination string, limit int64) (finalErr error) {
	if err := requirePrivateFile(source); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, named) || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("source file changed or is not private")
	}
	if limit > 0 && info.Size() > limit {
		return fmt.Errorf("recovery material exceeds size limit")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
	}()
	buffer := make([]byte, 64<<10)
	defer clear(buffer)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := input.Read(buffer)
		if n > 0 {
			copied += int64(n)
			if copied > info.Size() {
				return fmt.Errorf("source file changed while copying")
			}
			if _, err := output.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := input.Stat()
	if err != nil {
		return err
	}
	if copied != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("source file changed while copying")
	}
	return output.Sync()
}
