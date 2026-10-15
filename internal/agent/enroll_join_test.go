package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

type fakeBootstrapEnrollmentRemote struct {
	legacy *fakeEnrollmentRemote
	fail   error
	calls  []enrollmentCall
}

func (f *fakeBootstrapEnrollmentRemote) EnrollAgent(ctx context.Context, artifact JoinArtifact, csr []byte, key string) (*orchv1alpha1.EnrollmentServiceEnrollAgentResponse, error) {
	f.calls = append(f.calls, enrollmentCall{name: artifact.NodeName, csr: append([]byte(nil), csr...), key: key})
	if f.fail != nil {
		return nil, f.fail
	}
	response, err := f.legacy.EnrollAgent(ctx, artifact.NodeName, csr, key)
	if err != nil {
		return nil, err
	}
	return &orchv1alpha1.EnrollmentServiceEnrollAgentResponse{
		NodeId: response.GetNodeId(), NodeName: response.GetNodeName(), ClusterId: response.GetClusterId(),
		ControllerEndpoint: response.GetControllerEndpoint(), CaCertificatePem: response.GetCaCertificatePem(),
		AgentCertificatePem: response.GetAgentCertificatePem(), ExpiresAt: response.GetExpiresAt(),
		Changed: response.GetChanged(), EventSequence: response.GetEventSequence(),
	}, nil
}
func (*fakeBootstrapEnrollmentRemote) CloseIdleConnections() {}

func TestJoinedEnrollmentResumesWithoutAdministratorCredentials(t *testing.T) {
	t.Parallel()
	root, _, legacy := enrollmentFixture(t)
	artifact := JoinArtifact{Version: JoinArtifactVersion, JoinID: "join_0123456789abcdef0123456789abcdef", Secret: bytes.Repeat([]byte{9}, 32),
		NodeName: "node-1", ClusterID: legacy.clusterID, ControllerEndpoint: legacy.server, CACertificatePEM: legacy.caPEM, ExpiresAt: enrollmentTestTime.Add(15 * time.Minute)}
	encoded, err := EncodeJoinArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	remote := &fakeBootstrapEnrollmentRemote{legacy: legacy, fail: errors.New("uncertain")}
	dataDir := filepath.Join(root, "joined-agent")
	options := JoinEnrollOptions{JoinFile: "-", JoinInput: bytes.NewReader(encoded), DataDir: dataDir, Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader, Remote: remote}
	if _, err := EnrollWithJoin(context.Background(), options); err == nil {
		t.Fatal("uncertain enrollment succeeded")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, ConfigFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed enrollment published config: %v", err)
	}
	remote.fail = nil
	options.JoinInput = bytes.NewReader(encoded)
	result, err := EnrollWithJoin(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Config.NodeName != "node-1" || len(remote.calls) != 2 {
		t.Fatalf("result = %#v calls=%d", result, len(remote.calls))
	}
	if !bytes.Equal(remote.calls[0].csr, remote.calls[1].csr) || remote.calls[0].key != remote.calls[1].key {
		t.Fatal("joined retry changed CSR or idempotency key")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, EnrollmentFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed stage remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "operator", "context.json")); err != nil {
		t.Fatal(err)
	}
}

func TestJoinArtifactRejectsUnsafeInput(t *testing.T) {
	t.Parallel()
	_, _, legacy := enrollmentFixture(t)
	artifact := JoinArtifact{Version: JoinArtifactVersion, JoinID: "join_0123456789abcdef0123456789abcdef", Secret: bytes.Repeat([]byte{1}, 32), NodeName: "node-1",
		ClusterID: legacy.clusterID, ControllerEndpoint: legacy.server, CACertificatePEM: legacy.caPEM, ExpiresAt: enrollmentTestTime.Add(time.Minute)}
	encoded, _ := EncodeJoinArtifact(artifact)
	if _, err := ParseJoinArtifact(append(encoded, []byte("{}")...), enrollmentTestTime); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	artifact.ExpiresAt = enrollmentTestTime
	expired, _ := EncodeJoinArtifact(artifact)
	if _, err := ParseJoinArtifact(expired, enrollmentTestTime); err == nil {
		t.Fatal("expired artifact accepted")
	}
	path := filepath.Join(t.TempDir(), "join.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJoinArtifact(path, nil, enrollmentTestTime); err == nil {
		t.Fatal("insecure join file accepted")
	}
}
