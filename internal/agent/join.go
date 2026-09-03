package agent

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

const JoinArtifactVersion = "v1alpha1"

type JoinArtifact struct {
	Version            string    `json:"version"`
	JoinID             string    `json:"join_id"`
	Secret             []byte    `json:"secret"`
	NodeName           string    `json:"node_name"`
	ClusterID          string    `json:"cluster_id"`
	ControllerEndpoint string    `json:"controller_endpoint"`
	CACertificatePEM   []byte    `json:"ca_certificate_pem"`
	ExpiresAt          time.Time `json:"expires_at"`
}

func ParseJoinArtifact(contents []byte, now time.Time) (JoinArtifact, error) {
	if len(contents) == 0 || len(contents) > 64<<10 {
		return JoinArtifact{}, fmt.Errorf("join artifact must contain 1-65536 bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var artifact JoinArtifact
	if err := decoder.Decode(&artifact); err != nil {
		return JoinArtifact{}, fmt.Errorf("decode join artifact: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return JoinArtifact{}, fmt.Errorf("decode join artifact trailing data")
	}
	if artifact.Version != JoinArtifactVersion || artifact.JoinID == "" || len(artifact.Secret) != 32 || artifact.NodeName == "" ||
		artifact.ClusterID == "" || artifact.ExpiresAt.IsZero() {
		return JoinArtifact{}, fmt.Errorf("join artifact is incomplete or has an unsupported version")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !now.UTC().Before(artifact.ExpiresAt.UTC()) {
		return JoinArtifact{}, fmt.Errorf("join artifact expired at %s", artifact.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	server, err := url.Parse(artifact.ControllerEndpoint)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || server.User != nil || server.Path != "" || server.RawQuery != "" || server.Fragment != "" {
		return JoinArtifact{}, fmt.Errorf("join artifact controller endpoint must be an HTTPS origin")
	}
	block, rest := pem.Decode(artifact.CACertificatePEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return JoinArtifact{}, fmt.Errorf("join artifact contains an invalid cluster CA")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return JoinArtifact{}, fmt.Errorf("join artifact contains an invalid cluster CA")
	}
	return artifact, nil
}

func ReadJoinArtifact(path string, input io.Reader, now time.Time) (JoinArtifact, error) {
	var contents []byte
	var err error
	if path == "-" {
		if input == nil {
			return JoinArtifact{}, fmt.Errorf("join artifact stdin is unavailable")
		}
		contents, err = io.ReadAll(io.LimitReader(input, (64<<10)+1))
	} else {
		if strings.TrimSpace(path) == "" {
			return JoinArtifact{}, fmt.Errorf("join artifact path is required")
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return JoinArtifact{}, statErr
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return JoinArtifact{}, fmt.Errorf("join artifact must be a private regular file")
		}
		contents, err = os.ReadFile(path)
	}
	if err != nil {
		return JoinArtifact{}, fmt.Errorf("read join artifact: %w", err)
	}
	return ParseJoinArtifact(contents, now)
}

func EncodeJoinArtifact(artifact JoinArtifact) ([]byte, error) {
	encoded, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode join artifact: %w", err)
	}
	return append(encoded, '\n'), nil
}
