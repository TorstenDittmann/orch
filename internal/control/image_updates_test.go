package control

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
)

func TestImageUpdatesFollowMovedTags(t *testing.T) {
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	var digit atomic.Int32
	digit.Store(1)
	var fail atomic.Bool
	s := newService(state, cluster, nil, nil, nil, nil, time.Now)
	s.imageResolver = imageResolverFunc(func(context.Context, string, string, []byte) (string, error) {
		if fail.Load() {
			return "", images.ErrUnavailable
		}
		return "docker.io/library/busybox@sha256:" + strings.Repeat(string(rune('0'+digit.Load())), 64), nil
	})
	manifest := newTestManifest("hello", map[string]config.Service{"worker": {Image: "busybox:latest"}})
	manifest.Deploy.AutoUpdate = true
	canonical := mustCanonicalConfig(t, manifest)
	first, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: canonical, IdempotencyKey: "image-updates-first"}))
	if err != nil {
		t.Fatal(err)
	}

	if changed, err := s.updateApplicationImages(ctx, "hello"); err != nil || changed {
		t.Fatalf("unchanged tag redeployed: %v %v", changed, err)
	}
	digit.Store(2)
	if changed, err := s.updateApplicationImages(ctx, "hello"); err != nil || !changed {
		t.Fatalf("moved tag not deployed: %v %v", changed, err)
	}
	history, _, err := state.Deployments(ctx, "hello", 0, 10)
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %d %v", len(history), err)
	}
	latest := history[0]
	if latest.Generation != first.Msg.GetGeneration()+1 || string(latest.RequestedManifest) != string(canonical) ||
		!strings.Contains(string(latest.CanonicalManifest), strings.Repeat("2", 64)) || latest.Source == nil || latest.Source.Actor != imageUpdateActor {
		t.Fatalf("auto update deployment = %+v", latest)
	}
	if changed, err := s.updateApplicationImages(ctx, "hello"); err != nil || changed {
		t.Fatalf("same digest redeployed: %v %v", changed, err)
	}
	fail.Store(true)
	if changed, err := s.updateApplicationImages(ctx, "hello"); err == nil || changed {
		t.Fatalf("registry failure = %v %v", changed, err)
	}
	fail.Store(false)

	// A rollback pauses updates until the next deploy, even when the tag moved.
	if _, err := s.RollbackApplication(ctx, connect.NewRequest(&orchv1alpha1.RollbackApplicationRequest{Application: "hello", RevisionId: first.Msg.GetRevisionId(), ExpectedGeneration: latest.Generation, IdempotencyKey: "image-updates-rollback"})); err != nil {
		t.Fatal(err)
	}
	digit.Store(3)
	if changed, err := s.updateApplicationImages(ctx, "hello"); err != nil || changed {
		t.Fatalf("update undid a rollback: %v %v", changed, err)
	}
}

func TestImageUpdatesRequireOptIn(t *testing.T) {
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	var calls atomic.Int32
	s := newService(state, cluster, nil, nil, nil, nil, time.Now)
	s.imageResolver = imageResolverFunc(func(context.Context, string, string, []byte) (string, error) {
		n := calls.Add(1)
		return "docker.io/library/busybox@sha256:" + strings.Repeat(string(rune('0'+n)), 64), nil
	})
	canonical := mustCanonicalConfig(t, newTestManifest("hello", map[string]config.Service{"worker": {Image: "busybox:latest"}}))
	if _, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: canonical, IdempotencyKey: "image-updates-off"})); err != nil {
		t.Fatal(err)
	}
	s.updateImages(ctx)
	if calls.Load() != 1 {
		t.Fatal("application without autoUpdate contacted the registry")
	}
	if strings.Contains(string(canonical), "auto_update") {
		t.Fatal("default manifest changed its canonical form")
	}
}

func TestImageUpdatesYieldToNewerDeploys(t *testing.T) {
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	var digit atomic.Int32
	digit.Store(1)
	s := newService(state, cluster, nil, nil, nil, nil, time.Now)
	s.imageResolver = imageResolverFunc(func(context.Context, string, string, []byte) (string, error) {
		return "docker.io/library/busybox@sha256:" + strings.Repeat(string(rune('0'+digit.Load())), 64), nil
	})
	manifest := newTestManifest("hello", map[string]config.Service{"worker": {Image: "busybox:latest"}})
	manifest.Deploy.AutoUpdate = true
	if _, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: mustCanonicalConfig(t, manifest), IdempotencyKey: "image-updates-race"})); err != nil {
		t.Fatal(err)
	}
	history, _, err := state.Deployments(ctx, "hello", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	current := history[0]
	manifest.Services["worker"] = config.Service{Image: "busybox:other", Replicas: 1, Restart: config.DefaultRestart, Resources: manifest.Services["worker"].Resources}
	if _, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: mustCanonicalConfig(t, manifest), IdempotencyKey: "image-updates-newer"})); err != nil {
		t.Fatal(err)
	}
	digit.Store(2)
	resolved := []byte(strings.ReplaceAll(string(current.CanonicalManifest), strings.Repeat("1", 64), strings.Repeat("2", 64)))
	digest := config.DigestCanonicalJSON(resolved)
	_, err = state.ApplyApplication(ctx, model.ApplyApplication{
		Name: "hello", RevisionID: newRevisionID(cluster.ID, "image-updates-stale", digest), Digest: digest,
		CanonicalManifest: resolved, RequestedManifest: current.RequestedManifest, IdempotencyKey: "image-updates-stale",
		CreatedAt: time.Now().UTC(), ExpectedGeneration: current.Generation,
	})
	if !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("stale image update replaced a newer deploy: %v", err)
	}
}
