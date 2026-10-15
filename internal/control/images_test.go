package control

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
	"google.golang.org/protobuf/proto"
)

type imageResolverFunc func(context.Context, string, string, []byte) (string, error)

func (f imageResolverFunc) Resolve(ctx context.Context, image, user string, secret []byte) (string, error) {
	return f(ctx, image, user, secret)
}

func TestTagDeployReplayMovementAndRollback(t *testing.T) {
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	var calls atomic.Int32
	var fail atomic.Bool
	image1 := "docker.io/library/busybox@sha256:" + strings.Repeat("1", 64)
	image2 := "docker.io/library/busybox@sha256:" + strings.Repeat("2", 64)
	s := newService(state, cluster, nil, nil, nil, nil, time.Now)
	s.imageResolver = imageResolverFunc(func(_ context.Context, image, user string, secret []byte) (string, error) {
		n := calls.Add(1)
		if fail.Load() {
			return "", images.ErrUnavailable
		}
		if image != "docker.io/library/busybox:latest" || user != "" || len(secret) != 0 {
			t.Error("unexpected resolver input")
		}
		if n == 1 {
			return image1, nil
		}
		return image2, nil
	})
	manifest := newTestManifest("hello", map[string]config.Service{"worker": {Image: "busybox:latest"}})
	manifest.CronJobs = map[string]config.CronJob{"backup": {Image: "docker.io/library/busybox:latest", Schedule: "*/2 * * * *", Resources: config.Resources{CPU: "100m", Memory: "64Mi"}, StartDeadline: "30s", Timeout: "5m"}}
	canonical := mustCanonicalConfig(t, manifest)
	request := func(key string) *connect.Request[orchv1alpha1.ApplyApplicationRequest] {
		return connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: canonical, IdempotencyKey: key})
	}
	first, err := s.ApplyApplication(ctx, request("image-deployment-one"))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || first.Msg.GetDigest() != config.DigestCanonicalJSON(canonical) {
		t.Fatal("duplicate image lookup or changed receipt contract")
	}
	second, err := s.ApplyApplication(ctx, request("image-deployment-two"))
	if err != nil || second.Msg.GetGeneration() != 2 {
		t.Fatalf("moved tag: %v %v", second, err)
	}
	fail.Store(true)
	replay, err := s.ApplyApplication(ctx, request("image-deployment-one"))
	if err != nil || !proto.Equal(first.Msg, replay.Msg) || calls.Load() != 2 {
		t.Fatalf("replay contacted registry or changed: %v", err)
	}
	d, err := state.Deployment(ctx, "hello", first.Msg.GetRevisionId())
	if err != nil {
		t.Fatal(err)
	}
	message := deploymentMessage(d)
	if len(message.Images) != 2 {
		t.Fatal("missing service/cron images")
	}
	for _, image := range message.Images {
		if image.Image != image1 || !strings.HasSuffix(image.RequestedImage, ":latest") {
			t.Fatalf("image metadata = %v", image)
		}
	}
	rollback, err := s.RollbackApplication(ctx, connect.NewRequest(&orchv1alpha1.RollbackApplicationRequest{Application: "hello", RevisionId: first.Msg.GetRevisionId(), ExpectedGeneration: 2, IdempotencyKey: "image-rollback-one"}))
	if err != nil || rollback.Msg.GetDeployment().GetGeneration() != 3 || calls.Load() != 2 {
		t.Fatalf("rollback resolved again: %v", err)
	}
	rolled, err := state.Deployment(ctx, "hello", rollback.Msg.GetDeployment().GetRevisionId())
	if err != nil || string(rolled.CanonicalManifest) != string(d.CanonicalManifest) || string(rolled.RequestedManifest) != string(d.RequestedManifest) {
		t.Fatalf("rollback lost immutable intent: %v", err)
	}
	if _, err := s.ApplyApplication(ctx, request("image-deployment-three")); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("failed registry accepted: %v", err)
	}
	current, err := state.Application(ctx, "hello")
	if err != nil || current.Generation != 3 {
		t.Fatal("failed resolution advanced desired state")
	}
	changed := append([]byte(nil), canonical...)
	changed = []byte(strings.Replace(string(changed), "busybox:latest", "busybox:other", 1))
	_, err = s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: changed, IdempotencyKey: "image-deployment-one"}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists || calls.Load() != 3 {
		t.Fatalf("conflicting retry contacted registry: %v", err)
	}
}

func TestConcurrentTagRequestsShareFirstAcceptedResolution(t *testing.T) {
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	s := newService(state, cluster, nil, nil, nil, nil, time.Now)
	var calls atomic.Int32
	ready := make(chan struct{})
	s.imageResolver = imageResolverFunc(func(context.Context, string, string, []byte) (string, error) {
		n := calls.Add(1)
		if n == 2 {
			close(ready)
		}
		<-ready
		return "docker.io/library/busybox@sha256:" + strings.Repeat(string(rune('0'+n)), 64), nil
	})
	canonical := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{"worker": {Image: "busybox:latest"}}))
	var wg sync.WaitGroup
	results := make([]*orchv1alpha1.ApplyApplicationResponse, 2)
	for i := range results {
		wg.Go(func() {
			response, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: canonical, IdempotencyKey: "concurrent-image-request"}))
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = response.Msg
		})
	}
	wg.Wait()
	if results[0] == nil || !proto.Equal(results[0], results[1]) {
		t.Fatal("concurrent retries returned different resolutions")
	}
	history, _, err := state.Deployments(ctx, "hello", 0, 20)
	if err != nil || len(history) != 1 {
		t.Fatal("concurrent retry created multiple deployments")
	}
}

func TestImageCredentialScopeAndSanitizedFailure(t *testing.T) {
	state, cluster := reconcilerStore(t)
	box, err := security.NewSecretBox(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := state.SetRegistryCredential(ctx, box, "registry.example:5443", "robot", "regv_test", []byte("private-password"), "registry-test-key", time.Now()); err != nil {
		t.Fatal(err)
	}
	s := newService(state, cluster, nil, nil, box, nil, time.Now)
	var material []byte
	s.imageResolver = imageResolverFunc(func(_ context.Context, image, user string, secret []byte) (string, error) {
		if user != "robot" || string(secret) != "private-password" {
			t.Error("wrong credentials")
		}
		material = secret
		return "", errors.New("upstream error private-password")
	})
	manifest := newTestManifest("hello", map[string]config.Service{"worker": {Image: "registry.example:5443/team/app:latest"}})
	_, err = s.resolveImages(ctx, manifest)
	if connect.CodeOf(err) != connect.CodeUnavailable || strings.Contains(err.Error(), "private-password") || !strings.Contains(err.Error(), "services.worker.image") {
		t.Fatalf("unsafe or unhelpful error: %v", err)
	}
	for _, b := range material {
		if b != 0 {
			t.Fatal("credential buffer retained")
		}
	}
	if _, err := state.Application(ctx, "hello"); !errors.Is(err, store.ErrApplicationNotFound) {
		t.Fatal("resolution failure wrote application")
	}
}
