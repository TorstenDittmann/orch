package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	controlclient "change.codes/torsten/better-swarm/internal/client"
	clusterctl "change.codes/torsten/better-swarm/internal/cluster"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/security"
)

type EnrollOptions struct {
	Name        string
	ContextPath string
	DataDir     string
	Now         func() time.Time
	Random      io.Reader
	Remote      EnrollmentRemote
}

type EnrollmentRemote interface {
	EnrollAgent(context.Context, string, []byte, string) (*orchv1alpha1.EnrollAgentResponse, error)
	CloseIdleConnections()
}

type EnrollResult struct {
	Config  Config
	DataDir string
	Created bool
}

type enrollmentStage struct {
	Version        int    `json:"version"`
	NodeName       string `json:"node_name"`
	ClusterID      string `json:"cluster_id"`
	Server         string `json:"server"`
	IdempotencyKey string `json:"idempotency_key"`
	CSRDER         []byte `json:"certificate_request_der"`
	PrivateKeyPEM  []byte `json:"private_key_pem"`
}

// Enroll creates or resumes a CSR-based one-node enrollment. agent.json is
// published last, so an interrupted attempt never looks complete.
func Enroll(ctx context.Context, options EnrollOptions) (EnrollResult, error) {
	if err := config.ValidateApplicationName(options.Name); err != nil {
		return EnrollResult{}, fmt.Errorf("invalid agent name: %w", err)
	}
	dataDir, err := normalizeDataDir(options.DataDir)
	if err != nil {
		return EnrollResult{}, err
	}
	contextPath, err := filepath.Abs(options.ContextPath)
	if err != nil || strings.TrimSpace(options.ContextPath) == "" {
		return EnrollResult{}, fmt.Errorf("administrator context path is required")
	}
	contextPath = filepath.Clean(contextPath)
	if pathContains(dataDir, contextPath) {
		return EnrollResult{}, fmt.Errorf("agent data directory must not contain the administrator context")
	}
	local, err := clusterctl.LoadContext(contextPath)
	if err != nil {
		return EnrollResult{}, err
	}
	if _, err := os.Lstat(filepath.Join(dataDir, ConfigFilename)); err == nil {
		credentials, loadErr := Load(dataDir, currentTime(options.Now))
		if loadErr != nil {
			return EnrollResult{}, loadErr
		}
		if credentials.Config.NodeName != options.Name || credentials.Config.ClusterID != local.Cluster.ID {
			return EnrollResult{}, fmt.Errorf("existing agent identity belongs to a different node or cluster")
		}
		return EnrollResult{Config: credentials.Config, DataDir: dataDir, Created: false}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return EnrollResult{}, fmt.Errorf("inspect existing agent config: %w", err)
	}
	if err := prepareLayout(dataDir); err != nil {
		return EnrollResult{}, err
	}

	randomSource := options.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	stagePath := filepath.Join(dataDir, EnrollmentFilename)
	stage, err := loadEnrollmentStage(stagePath)
	if errors.Is(err, os.ErrNotExist) {
		csrDER, keyPEM, _, generateErr := security.GenerateAgentCertificateRequest(options.Name, randomSource)
		if generateErr != nil {
			return EnrollResult{}, generateErr
		}
		key, generateErr := newEnrollmentKey(randomSource)
		if generateErr != nil {
			return EnrollResult{}, generateErr
		}
		stage = enrollmentStage{
			Version: ConfigVersion, NodeName: options.Name, ClusterID: local.Cluster.ID,
			Server: local.Server, IdempotencyKey: key, CSRDER: csrDER, PrivateKeyPEM: keyPEM,
		}
		encoded, encodeErr := json.MarshalIndent(stage, "", "  ")
		if encodeErr != nil {
			return EnrollResult{}, fmt.Errorf("encode enrollment stage: %w", encodeErr)
		}
		if err := writePrivateAtomic(stagePath, append(encoded, '\n')); err != nil {
			return EnrollResult{}, fmt.Errorf("persist enrollment stage: %w", err)
		}
	} else if err != nil {
		return EnrollResult{}, err
	}
	if stage.Version != ConfigVersion || stage.NodeName != options.Name || stage.ClusterID != local.Cluster.ID || stage.Server != local.Server {
		return EnrollResult{}, fmt.Errorf("staged enrollment belongs to a different node, cluster, or endpoint")
	}
	if err := security.VerifyAgentCertificateRequestKey(stage.CSRDER, stage.PrivateKeyPEM); err != nil {
		return EnrollResult{}, fmt.Errorf("validate staged enrollment: %w", err)
	}

	remote := options.Remote
	closeRemote := false
	if remote == nil {
		created, err := controlclient.New(contextPath)
		if err != nil {
			return EnrollResult{}, err
		}
		remote = created
		closeRemote = true
	}
	if closeRemote {
		defer remote.CloseIdleConnections()
	}
	response, err := remote.EnrollAgent(ctx, stage.NodeName, stage.CSRDER, stage.IdempotencyKey)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("enroll agent; retry with the same data directory: %w", err)
	}
	if err := validateEnrollmentResponse(response, stage, currentTime(options.Now)); err != nil {
		return EnrollResult{}, err
	}
	identityDir := filepath.Join(dataDir, IdentityDirectory)
	if err := publishPrivateExact(filepath.Join(identityDir, CACertificateFile), response.GetCaCertificatePem()); err != nil {
		return EnrollResult{}, fmt.Errorf("publish agent CA certificate: %w", err)
	}
	if err := publishPrivateExact(filepath.Join(identityDir, AgentCertificateFile), response.GetAgentCertificatePem()); err != nil {
		return EnrollResult{}, fmt.Errorf("publish agent certificate: %w", err)
	}
	if err := publishPrivateExact(filepath.Join(identityDir, AgentPrivateKeyFile), stage.PrivateKeyPEM); err != nil {
		return EnrollResult{}, fmt.Errorf("publish agent private key: %w", err)
	}
	resultConfig := Config{
		Version: ConfigVersion, NodeID: response.GetNodeId(), NodeName: response.GetNodeName(),
		ClusterID: response.GetClusterId(), Server: response.GetControllerEndpoint(),
	}
	encoded, err := json.MarshalIndent(resultConfig, "", "  ")
	if err != nil {
		return EnrollResult{}, fmt.Errorf("encode agent config: %w", err)
	}
	if err := writePrivateAtomic(filepath.Join(dataDir, ConfigFilename), append(encoded, '\n')); err != nil {
		return EnrollResult{}, fmt.Errorf("publish agent config: %w", err)
	}
	if err := os.Remove(stagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return EnrollResult{}, fmt.Errorf("remove completed enrollment stage: %w", err)
	}
	return EnrollResult{Config: resultConfig, DataDir: dataDir, Created: true}, nil
}

func validateEnrollmentResponse(response *orchv1alpha1.EnrollAgentResponse, stage enrollmentStage, now time.Time) error {
	if response == nil || response.GetNodeId() == "" || response.GetNodeName() != stage.NodeName ||
		response.GetClusterId() != stage.ClusterID || response.GetControllerEndpoint() != stage.Server {
		return fmt.Errorf("controller returned enrollment material for a different node, cluster, or endpoint")
	}
	if response.GetExpiresAt() == nil || response.GetExpiresAt().CheckValid() != nil || !response.GetExpiresAt().AsTime().After(now.UTC()) {
		return fmt.Errorf("controller returned an invalid or expired agent certificate")
	}
	if err := security.VerifyAgentIdentity(response.GetCaCertificatePem(), security.KeyPair{
		CertificatePEM: response.GetAgentCertificatePem(), PrivateKeyPEM: stage.PrivateKeyPEM,
	}, stage.ClusterID, response.GetNodeId(), now.UTC()); err != nil {
		return fmt.Errorf("validate enrolled agent identity: %w", err)
	}
	return nil
}

func loadEnrollmentStage(path string) (enrollmentStage, error) {
	contents, err := readPrivateFile(path)
	if err != nil {
		return enrollmentStage{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var stage enrollmentStage
	if err := decoder.Decode(&stage); err != nil {
		return enrollmentStage{}, fmt.Errorf("decode enrollment stage: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return enrollmentStage{}, fmt.Errorf("decode enrollment stage: multiple JSON values")
		}
		return enrollmentStage{}, fmt.Errorf("decode enrollment stage trailing data: %w", err)
	}
	return stage, nil
}

func prepareLayout(dataDir string) error {
	info, err := os.Lstat(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return fmt.Errorf("create agent data directory: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect agent data directory: %w", err)
	} else if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("agent data directory must be a private non-symlink directory")
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{IdentityDirectory, LogsDirectory, VolumesDirectory} {
		path := filepath.Join(dataDir, name)
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create agent %s directory: %w", name, err)
		}
		if err := requirePrivateDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

func writePrivateAtomic(path string, contents []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("refusing to replace insecure or non-regular file %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".orch-agent-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	remove = false
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// publishPrivateExact makes partially completed enrollment resumable without
// ever replacing different identity material already present at the target.
func publishPrivateExact(path string, contents []byte) error {
	existing, err := readPrivateFile(path)
	if err == nil {
		if !bytes.Equal(existing, contents) {
			return fmt.Errorf("refusing to replace different identity material at %s", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writePrivateAtomic(path, contents)
}

func currentTime(now func() time.Time) time.Time {
	if now == nil {
		return time.Now().UTC()
	}
	return now().UTC()
}

func newEnrollmentKey(source io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", fmt.Errorf("generate enrollment idempotency key: %w", err)
	}
	return "agent_" + hex.EncodeToString(value), nil
}

func pathContains(directory, candidate string) bool {
	relative, err := filepath.Rel(directory, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
