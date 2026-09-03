package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	"change.codes/torsten/better-swarm/internal/security"
	"connectrpc.com/connect"
)

type JoinEnrollOptions struct {
	JoinFile  string
	JoinInput io.Reader
	DataDir   string
	Now       func() time.Time
	Random    io.Reader
	Remote    BootstrapEnrollmentRemote
}

type BootstrapEnrollmentRemote interface {
	EnrollAgent(context.Context, JoinArtifact, []byte, string) (*orchv1alpha1.EnrollmentServiceEnrollAgentResponse, error)
	CloseIdleConnections()
}

type joinEnrollmentStage struct {
	Version        int          `json:"version"`
	Artifact       JoinArtifact `json:"artifact"`
	IdempotencyKey string       `json:"idempotency_key"`
	CSRDER         []byte       `json:"certificate_request_der"`
	PrivateKeyPEM  []byte       `json:"private_key_pem"`
}

func EnrollWithJoin(ctx context.Context, options JoinEnrollOptions) (EnrollResult, error) {
	now := currentTime(options.Now)
	artifact, err := ReadJoinArtifact(options.JoinFile, options.JoinInput, now)
	if err != nil {
		return EnrollResult{}, err
	}
	dataDir, err := normalizeDataDir(options.DataDir)
	if err != nil {
		return EnrollResult{}, err
	}
	if _, err := os.Lstat(filepath.Join(dataDir, ConfigFilename)); err == nil {
		credentials, loadErr := Load(dataDir, now)
		if loadErr != nil {
			return EnrollResult{}, loadErr
		}
		if credentials.Config.NodeName != artifact.NodeName || credentials.Config.ClusterID != artifact.ClusterID || credentials.Config.Server != artifact.ControllerEndpoint {
			return EnrollResult{}, fmt.Errorf("existing agent identity belongs to a different node, cluster, or endpoint")
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
	stage, err := loadJoinEnrollmentStage(stagePath)
	if errors.Is(err, os.ErrNotExist) {
		csrDER, keyPEM, _, generateErr := security.GenerateAgentCertificateRequest(artifact.NodeName, randomSource)
		if generateErr != nil {
			return EnrollResult{}, generateErr
		}
		key, generateErr := newEnrollmentKey(randomSource)
		if generateErr != nil {
			return EnrollResult{}, generateErr
		}
		stage = joinEnrollmentStage{Version: 2, Artifact: artifact, IdempotencyKey: key, CSRDER: csrDER, PrivateKeyPEM: keyPEM}
		encoded, encodeErr := json.MarshalIndent(stage, "", "  ")
		if encodeErr != nil {
			return EnrollResult{}, fmt.Errorf("encode joined enrollment stage: %w", encodeErr)
		}
		if err := writePrivateAtomic(stagePath, append(encoded, '\n')); err != nil {
			return EnrollResult{}, fmt.Errorf("persist joined enrollment stage: %w", err)
		}
	} else if err != nil {
		return EnrollResult{}, err
	}
	if stage.Version != 2 || stage.Artifact.JoinID != artifact.JoinID || stage.Artifact.NodeName != artifact.NodeName ||
		stage.Artifact.ClusterID != artifact.ClusterID || stage.Artifact.ControllerEndpoint != artifact.ControllerEndpoint ||
		!equalBytes(stage.Artifact.Secret, artifact.Secret) {
		return EnrollResult{}, fmt.Errorf("staged enrollment belongs to a different join artifact")
	}
	if err := security.VerifyAgentCertificateRequestKey(stage.CSRDER, stage.PrivateKeyPEM); err != nil {
		return EnrollResult{}, fmt.Errorf("validate staged joined enrollment: %w", err)
	}
	remote := options.Remote
	closeRemote := false
	if remote == nil {
		remote, err = newBootstrapClient(stage.Artifact)
		if err != nil {
			return EnrollResult{}, err
		}
		closeRemote = true
	}
	if closeRemote {
		defer remote.CloseIdleConnections()
	}
	response, err := remote.EnrollAgent(ctx, stage.Artifact, stage.CSRDER, stage.IdempotencyKey)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("enroll agent with join credential; retry with the same artifact and data directory: %w", err)
	}
	legacyStage := enrollmentStage{Version: ConfigVersion, NodeName: artifact.NodeName, ClusterID: artifact.ClusterID,
		Server: artifact.ControllerEndpoint, IdempotencyKey: stage.IdempotencyKey, CSRDER: stage.CSRDER, PrivateKeyPEM: stage.PrivateKeyPEM}
	legacyResponse := &orchv1alpha1.EnrollAgentResponse{
		NodeId: response.GetNodeId(), NodeName: response.GetNodeName(), ClusterId: response.GetClusterId(),
		ControllerEndpoint: response.GetControllerEndpoint(), CaCertificatePem: response.GetCaCertificatePem(),
		AgentCertificatePem: response.GetAgentCertificatePem(), ExpiresAt: response.GetExpiresAt(),
		Changed: response.GetChanged(), EventSequence: response.GetEventSequence(),
	}
	if err := validateEnrollmentResponse(legacyResponse, legacyStage, now); err != nil {
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
	resultConfig := Config{Version: ConfigVersion, NodeID: response.GetNodeId(), NodeName: response.GetNodeName(), ClusterID: response.GetClusterId(), Server: response.GetControllerEndpoint()}
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

func loadJoinEnrollmentStage(path string) (joinEnrollmentStage, error) {
	contents, err := readPrivateFile(path)
	if err != nil {
		return joinEnrollmentStage{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var stage joinEnrollmentStage
	if err := decoder.Decode(&stage); err != nil {
		return joinEnrollmentStage{}, fmt.Errorf("decode joined enrollment stage: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return joinEnrollmentStage{}, fmt.Errorf("decode joined enrollment stage trailing data")
	}
	return stage, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

type bootstrapClient struct {
	service orchv1alpha1connect.EnrollmentServiceClient
	http    *http.Client
}

func newBootstrapClient(artifact JoinArtifact) (*bootstrapClient, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(artifact.CACertificatePEM) {
		return nil, fmt.Errorf("load join artifact CA certificate")
	}
	server, _ := url.Parse(artifact.ControllerEndpoint)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: server.Hostname(), VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
			return errors.New("controller certificate was not verified")
		}
		return security.VerifyPeerIdentity(state.PeerCertificates[0], artifact.ClusterID, security.RoleController)
	}}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 60 * time.Second}
	httpClient := &http.Client{Transport: transport}
	return &bootstrapClient{service: orchv1alpha1connect.NewEnrollmentServiceClient(httpClient, artifact.ControllerEndpoint,
		connect.WithSendMaxBytes(64<<10), connect.WithReadMaxBytes(64<<10)), http: httpClient}, nil
}

func (c *bootstrapClient) EnrollAgent(ctx context.Context, artifact JoinArtifact, csr []byte, key string) (*orchv1alpha1.EnrollmentServiceEnrollAgentResponse, error) {
	response, err := c.service.EnrollAgent(ctx, connect.NewRequest(&orchv1alpha1.EnrollmentServiceEnrollAgentRequest{JoinId: artifact.JoinID, Secret: append([]byte(nil), artifact.Secret...), Name: artifact.NodeName, CertificateRequestDer: append([]byte(nil), csr...), IdempotencyKey: key}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}
func (c *bootstrapClient) CloseIdleConnections() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}
