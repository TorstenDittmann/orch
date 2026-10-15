package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/security"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var enrollmentTestTime = time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)

type enrollmentCall struct {
	name string
	csr  []byte
	key  string
}

type fakeEnrollmentRemote struct {
	caPEM     []byte
	caKey     []byte
	clusterID string
	server    string
	fail      error
	calls     []enrollmentCall
}

func (f *fakeEnrollmentRemote) EnrollAgent(_ context.Context, name string, csr []byte, key string) (*orchv1alpha1.EnrollAgentResponse, error) {
	f.calls = append(f.calls, enrollmentCall{name: name, csr: append([]byte(nil), csr...), key: key})
	if f.fail != nil {
		return nil, f.fail
	}
	hash, err := security.AgentCSRPublicKeyHash(csr)
	if err != nil {
		return nil, err
	}
	nodeID := "node_" + hash[:32]
	signed, err := security.SignAgentCertificate(f.caPEM, f.caKey, f.clusterID, nodeID, csr, enrollmentTestTime, rand.Reader)
	if err != nil {
		return nil, err
	}
	return &orchv1alpha1.EnrollAgentResponse{
		NodeId: nodeID, NodeName: name, ClusterId: f.clusterID, ControllerEndpoint: f.server,
		CaCertificatePem: f.caPEM, AgentCertificatePem: signed.CertificatePEM,
		ExpiresAt: timestamppb.New(signed.NotAfter), Changed: true, EventSequence: 2,
	}, nil
}

func (*fakeEnrollmentRemote) CloseIdleConnections() {}

func TestEnrollmentResumesSameCSRAndPublishesProtectedIdentityLast(t *testing.T) {
	t.Parallel()
	root, contextPath, remote := enrollmentFixture(t)
	dataDir := filepath.Join(root, "agent")
	remote.fail = errors.New("uncertain network failure")
	options := EnrollOptions{Name: "node-1", ContextPath: contextPath, DataDir: dataDir, Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader, Remote: remote}
	if _, err := Enroll(context.Background(), options); err == nil || !strings.Contains(err.Error(), "retry with the same data directory") {
		t.Fatalf("first Enroll() = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, ConfigFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed enrollment published config: %v", err)
	}
	stageBytes, err := os.ReadFile(filepath.Join(dataDir, EnrollmentFilename))
	if err != nil {
		t.Fatal(err)
	}
	var stage enrollmentStage
	if err := json.Unmarshal(stageBytes, &stage); err != nil {
		t.Fatal(err)
	}
	if len(stage.PrivateKeyPEM) == 0 || len(remote.calls) != 1 || bytes.Contains(remote.calls[0].csr, stage.PrivateKeyPEM) {
		t.Fatalf("staged/request identity is invalid")
	}

	remote.fail = nil
	result, err := Enroll(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Config.NodeName != "node-1" || len(remote.calls) != 2 {
		t.Fatalf("result = %#v, calls = %d", result, len(remote.calls))
	}
	if !bytes.Equal(remote.calls[0].csr, remote.calls[1].csr) || remote.calls[0].key != remote.calls[1].key {
		t.Fatal("retry did not reuse the exact CSR and idempotency key")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, EnrollmentFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed enrollment stage remains: %v", err)
	}
	credentials, err := Load(dataDir, enrollmentTestTime)
	if err != nil || credentials.Config != result.Config {
		t.Fatalf("Load() = %#v, %v", credentials.Config, err)
	}
	for _, path := range []string{
		dataDir, filepath.Join(dataDir, IdentityDirectory), filepath.Join(dataDir, LogsDirectory), filepath.Join(dataDir, VolumesDirectory),
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s = %#v, %v", path, info, err)
		}
	}
	for _, path := range []string{
		filepath.Join(dataDir, ConfigFilename), filepath.Join(dataDir, IdentityDirectory, CACertificateFile),
		filepath.Join(dataDir, IdentityDirectory, AgentCertificateFile), filepath.Join(dataDir, IdentityDirectory, AgentPrivateKeyFile),
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s = %#v, %v", path, info, err)
		}
	}

	callCount := len(remote.calls)
	repeated, err := Enroll(context.Background(), options)
	if err != nil || repeated.Created || !reflect.DeepEqual(repeated.Config, result.Config) || len(remote.calls) != callCount {
		t.Fatalf("repeated Enroll() = %#v, %v; calls %d", repeated, err, len(remote.calls))
	}
}

func TestEnrollmentFailsClosedOnPathsAndPartialIdentityMismatch(t *testing.T) {
	t.Parallel()
	t.Run("context overlap", func(t *testing.T) {
		root, contextPath, remote := enrollmentFixture(t)
		if _, err := Enroll(context.Background(), EnrollOptions{
			Name: "node-1", ContextPath: contextPath, DataDir: root,
			Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader, Remote: remote,
		}); err == nil || !strings.Contains(err.Error(), "must not contain") {
			t.Fatalf("Enroll() = %v", err)
		}
	})
	t.Run("insecure directory", func(t *testing.T) {
		root, contextPath, remote := enrollmentFixture(t)
		dataDir := filepath.Join(root, "agent")
		if err := os.Mkdir(dataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Enroll(context.Background(), EnrollOptions{
			Name: "node-1", ContextPath: contextPath, DataDir: dataDir,
			Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader, Remote: remote,
		}); err == nil || !strings.Contains(err.Error(), "private") {
			t.Fatalf("Enroll() = %v", err)
		}
	})
	t.Run("symlink directory", func(t *testing.T) {
		root, contextPath, remote := enrollmentFixture(t)
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		dataDir := filepath.Join(root, "agent")
		if err := os.Symlink(target, dataDir); err != nil {
			t.Fatal(err)
		}
		if _, err := Enroll(context.Background(), EnrollOptions{
			Name: "node-1", ContextPath: contextPath, DataDir: dataDir,
			Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader, Remote: remote,
		}); err == nil {
			t.Fatal("Enroll() accepted a symlink data directory")
		}
	})
	t.Run("different partial identity", func(t *testing.T) {
		root, contextPath, remote := enrollmentFixture(t)
		dataDir := filepath.Join(root, "agent")
		remote.fail = errors.New("offline")
		options := EnrollOptions{Name: "node-1", ContextPath: contextPath, DataDir: dataDir, Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader, Remote: remote}
		if _, err := Enroll(context.Background(), options); err == nil {
			t.Fatal("first Enroll() succeeded")
		}
		if err := os.WriteFile(filepath.Join(dataDir, IdentityDirectory, CACertificateFile), []byte("different\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		remote.fail = nil
		if _, err := Enroll(context.Background(), options); err == nil || !strings.Contains(err.Error(), "refusing to replace different") {
			t.Fatalf("resumed Enroll() = %v", err)
		}
	})
}

func TestLoadRejectsTrailingConfigAndInsecureKey(t *testing.T) {
	t.Parallel()
	root, contextPath, remote := enrollmentFixture(t)
	dataDir := filepath.Join(root, "agent")
	options := EnrollOptions{Name: "node-1", ContextPath: contextPath, DataDir: dataDir, Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader, Remote: remote}
	if _, err := Enroll(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dataDir, ConfigFilename)
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(contents, []byte("garbage")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dataDir, enrollmentTestTime); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("Load(trailing) = %v", err)
	}
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dataDir, IdentityDirectory, AgentPrivateKeyFile)
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dataDir, enrollmentTestTime); err == nil || !strings.Contains(err.Error(), "group or world") {
		t.Fatalf("Load(insecure key) = %v", err)
	}
}

func enrollmentFixture(t *testing.T) (string, string, *fakeEnrollmentRemote) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	result, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://127.0.0.1:9443", DataDir: dataDir, ContextPath: contextPath,
		Now: func() time.Time { return enrollmentTestTime }, Random: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dataDir, "pki", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := os.ReadFile(filepath.Join(dataDir, "pki", "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	return root, contextPath, &fakeEnrollmentRemote{
		caPEM: caPEM, caKey: caKey, clusterID: result.Cluster.ID, server: result.Cluster.Endpoint,
	}
}
