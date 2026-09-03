package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	"change.codes/torsten/better-swarm/internal/security"
	"connectrpc.com/connect"
)

const (
	renewalFilename    = ".certificate-renewal.json"
	renewalWindow      = 90 * 24 * time.Hour
	renewalJitterRange = 24 * time.Hour
)

var errCertificateRenewalDue = errors.New("agent certificate renewal is due")

type certificateRenewalStage struct {
	Version        int    `json:"version"`
	CSRDER         []byte `json:"certificate_request_der"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (r *sessionRunner) certificateRenewalDelay() time.Duration {
	due := r.credentials.Certificate.Leaf.NotAfter.Add(-renewalWindow).Add(renewalJitter(r.credentials.Config.NodeID))
	if r.renewalRetryAt.After(due) {
		due = r.renewalRetryAt
	}
	delay := due.Sub(currentTime(r.now))
	if delay <= 0 {
		return time.Nanosecond
	}
	return delay
}

func (r *sessionRunner) noteRenewalFailure() {
	if r.renewalBackoff == 0 {
		r.renewalBackoff = 5 * time.Minute
	} else {
		r.renewalBackoff *= 2
		if r.renewalBackoff > 6*time.Hour {
			r.renewalBackoff = 6 * time.Hour
		}
	}
	r.renewalRetryAt = currentTime(r.now).Add(r.renewalBackoff)
}

func renewalJitter(nodeID string) time.Duration {
	digest := sha256.Sum256([]byte(nodeID))
	return time.Duration(binary.BigEndian.Uint64(digest[:8]) % uint64(renewalJitterRange))
}

// maybeRenewCertificate reports whether it completed a renewal. A nil error
// with completed=false means the certificate is not due yet or a retry is
// deliberately deferred; callers must not clear retry state in that case.
func (r *sessionRunner) maybeRenewCertificate(ctx context.Context) (completed bool, err error) {
	if r.credentials.Certificate.Leaf == nil || r.certificateRenewalDelay() > time.Nanosecond {
		return false, nil
	}
	stagePath := filepath.Join(r.dataDir, renewalFilename)
	stage, err := loadCertificateRenewalStage(stagePath)
	if errors.Is(err, os.ErrNotExist) {
		csrDER, publicKeyHash, createErr := security.CreateAgentCertificateRequest(r.credentials.Config.NodeName, r.credentials.PrivateKeyPEM, r.random)
		if createErr != nil {
			return false, createErr
		}
		certificateHash, hashErr := security.CertificatePublicKeyHash(r.credentials.Certificate.Leaf)
		if hashErr != nil || certificateHash != publicKeyHash {
			return false, errors.New("renewal key does not match active agent certificate")
		}
		key, createErr := newEnrollmentKey(r.random)
		if createErr != nil {
			return false, createErr
		}
		stage = certificateRenewalStage{Version: 1, CSRDER: csrDER, IdempotencyKey: key}
		encoded, encodeErr := json.MarshalIndent(stage, "", "  ")
		if encodeErr != nil {
			return false, encodeErr
		}
		if err := writePrivateAtomic(stagePath, append(encoded, '\n')); err != nil {
			return false, fmt.Errorf("persist certificate renewal: %w", err)
		}
	} else if err != nil {
		return false, err
	}
	if stage.Version != 1 || len(stage.CSRDER) == 0 || stage.IdempotencyKey == "" {
		return false, errors.New("certificate renewal stage is invalid")
	}
	if err := security.VerifyAgentCertificateRequestKey(stage.CSRDER, r.credentials.PrivateKeyPEM); err != nil {
		return false, err
	}
	client, closeClient, err := r.renewalClient()
	if err != nil {
		return false, err
	}
	defer closeClient()
	response, err := client.RenewCertificate(ctx, connect.NewRequest(&orchv1alpha1.RenewCertificateRequest{CertificateRequestDer: stage.CSRDER, IdempotencyKey: stage.IdempotencyKey}))
	if err != nil {
		return false, err
	}
	if response.Msg.GetNodeId() != r.credentials.Config.NodeID || response.Msg.GetExpiresAt() == nil || response.Msg.GetExpiresAt().CheckValid() != nil {
		return false, errors.New("controller returned invalid renewed agent identity")
	}
	if err := security.VerifyAgentIdentity(r.credentials.CACertificatePEM, security.KeyPair{CertificatePEM: response.Msg.GetAgentCertificatePem(), PrivateKeyPEM: r.credentials.PrivateKeyPEM},
		r.credentials.Config.ClusterID, r.credentials.Config.NodeID, currentTime(r.now)); err != nil {
		return false, fmt.Errorf("validate renewed agent identity: %w", err)
	}
	if err := writePrivateAtomic(filepath.Join(r.dataDir, IdentityDirectory, AgentCertificateFile), response.Msg.GetAgentCertificatePem()); err != nil {
		return false, fmt.Errorf("publish renewed agent certificate: %w", err)
	}
	credentials, err := Load(r.dataDir, currentTime(r.now))
	if err != nil {
		return false, err
	}
	r.credentials = credentials
	if err := os.Remove(stagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove completed certificate renewal: %w", err)
	}
	return true, nil
}

func (r *sessionRunner) certificateExpired() bool {
	return r.credentials.Certificate.Leaf != nil && !currentTime(r.now).Before(r.credentials.Certificate.Leaf.NotAfter)
}

func loadCertificateRenewalStage(path string) (certificateRenewalStage, error) {
	contents, err := readPrivateFile(path)
	if err != nil {
		return certificateRenewalStage{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var stage certificateRenewalStage
	if err := decoder.Decode(&stage); err != nil {
		return certificateRenewalStage{}, fmt.Errorf("decode certificate renewal: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return certificateRenewalStage{}, errors.New("certificate renewal contains trailing data")
	}
	return stage, nil
}

func (r *sessionRunner) renewalClient() (orchv1alpha1connect.AgentServiceClient, func(), error) {
	tlsConfig := r.agentTLSConfig()
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 60 * time.Second}
	httpClient := &http.Client{Transport: transport}
	client := orchv1alpha1connect.NewAgentServiceClient(httpClient, r.credentials.Config.Server, connect.WithSendMaxBytes(20<<20), connect.WithReadMaxBytes(20<<20))
	return client, transport.CloseIdleConnections, nil
}

func (r *sessionRunner) agentTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: r.credentials.RootCAs,
		Certificates: []tls.Certificate{r.credentials.Certificate}, ServerName: r.credentials.ServerName,
		VerifyConnection: func(connection tls.ConnectionState) error {
			if len(connection.PeerCertificates) == 0 || len(connection.VerifiedChains) == 0 {
				return errors.New("controller certificate was not verified")
			}
			return security.VerifyPeerIdentity(connection.PeerCertificates[0], r.credentials.Config.ClusterID, security.RoleController)
		}}
}
