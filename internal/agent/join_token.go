package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
)

// JoinTokenPrefix starts every printable join token.
const JoinTokenPrefix = "orch1."

// joinToken is the printable form of a join artifact. It carries the CA
// fingerprint instead of the CA certificate; the joining node fetches the CA
// from the controller and accepts it only if the fingerprint matches.
type joinToken struct {
	Endpoint      string `json:"e"`
	ClusterID     string `json:"c"`
	CAFingerprint []byte `json:"f"`
	JoinID        string `json:"i"`
	Secret        []byte `json:"s"`
	NodeName      string `json:"n"`
	ExpiresAt     int64  `json:"x"`
}

// EncodeJoinToken renders a join artifact as a single copyable string.
func EncodeJoinToken(artifact JoinArtifact) (string, error) {
	validated, err := ParseJoinArtifact(mustJSON(artifact), time.Time{})
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(validated.CACertificatePEM)
	fingerprint := sha256.Sum256(block.Bytes)
	payload, err := json.Marshal(joinToken{
		Endpoint: validated.ControllerEndpoint, ClusterID: validated.ClusterID, CAFingerprint: fingerprint[:],
		JoinID: validated.JoinID, Secret: validated.Secret, NodeName: validated.NodeName,
		ExpiresAt: validated.ExpiresAt.UTC().Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("encode join token: %w", err)
	}
	return JoinTokenPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func decodeJoinToken(value string) (joinToken, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, JoinTokenPrefix) || len(value) > 4096 {
		return joinToken{}, errors.New("join token must start with " + JoinTokenPrefix)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, JoinTokenPrefix))
	if err != nil {
		return joinToken{}, errors.New("join token is malformed")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var token joinToken
	if err := decoder.Decode(&token); err != nil {
		return joinToken{}, errors.New("join token is malformed")
	}
	if token.Endpoint == "" || token.ClusterID == "" || len(token.CAFingerprint) != sha256.Size || token.JoinID == "" ||
		len(token.Secret) != 32 || token.NodeName == "" || token.ExpiresAt == 0 {
		return joinToken{}, errors.New("join token is incomplete")
	}
	return token, nil
}

// JoinTokenNodeName returns the node name a join token was issued for.
func JoinTokenNodeName(value string) (string, error) {
	token, err := decodeJoinToken(value)
	return token.NodeName, err
}

// ResolveJoinToken turns a join token into a full join artifact by fetching
// the cluster CA from the controller and pinning it to the token fingerprint.
func ResolveJoinToken(ctx context.Context, value string, now time.Time) (JoinArtifact, error) {
	token, err := decodeJoinToken(value)
	if err != nil {
		return JoinArtifact{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	expires := time.Unix(token.ExpiresAt, 0).UTC()
	if !now.Before(expires) {
		return JoinArtifact{}, fmt.Errorf("join token expired at %s; create a new one with orch join-token", expires.Format(time.RFC3339))
	}
	caPEM, err := fetchPinnedCA(ctx, token)
	if err != nil {
		return JoinArtifact{}, err
	}
	artifact := JoinArtifact{
		Version: JoinArtifactVersion, JoinID: token.JoinID, Secret: token.Secret, NodeName: token.NodeName,
		ClusterID: token.ClusterID, ControllerEndpoint: token.Endpoint, CACertificatePEM: caPEM, ExpiresAt: expires,
	}
	return ParseJoinArtifact(mustJSON(artifact), now)
}

func fetchPinnedCA(ctx context.Context, token joinToken) ([]byte, error) {
	caPEM, clusterID, err := security.FetchPinnedCA(ctx, token.Endpoint, token.CAFingerprint)
	if err != nil {
		if errors.Is(err, security.ErrPinnedCAMismatch) {
			return nil, errors.New("controller CA does not match the join token; check the controller address")
		}
		return nil, err
	}
	if clusterID != token.ClusterID {
		return nil, fmt.Errorf("validate controller identity: controller serves cluster %s, not %s", clusterID, token.ClusterID)
	}
	return caPEM, nil
}
