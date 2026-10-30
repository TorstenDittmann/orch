package control

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

func TestApplyGeneratesDatabaseSecretsOnce(t *testing.T) {
	state, cluster := reconcilerStore(t)
	box, err := security.NewSecretBox(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	s := newService(state, cluster, nil, nil, box, nil, time.Now)
	s.imageResolver = imageResolverFunc(func(_ context.Context, image, _ string, _ []byte) (string, error) {
		return "docker.io/library/postgres@sha256:" + strings.Repeat("1", 64), nil
	})
	manifest := newTestManifest("shop", map[string]config.Service{
		"main": {Image: "postgres:17", TCP: &config.TCP{Port: 5432}, SecretEnv: map[string]string{"POSTGRES_PASSWORD": "main-password"},
			Database: &config.Database{Engine: config.EnginePostgres, Version: "17"}},
	})
	canonical := mustCanonicalConfig(t, manifest)
	apply := func(key string) {
		t.Helper()
		if _, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: canonical, IdempotencyKey: key})); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		secret, err := state.ResolveApplicationSecret(ctx, box, "shop", name)
		if err != nil {
			t.Fatalf("secret %s: %v", name, err)
		}
		return string(secret.Value)
	}

	apply("database-deploy-one")
	password := read("main-password")
	if len(password) != 32 {
		t.Fatalf("password = %q", password)
	}
	if got, want := read("main-url"), "postgres://orch:"+password+"@main.shop.svc.internal:5432/main"; got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	apply("database-deploy-two")
	if read("main-password") != password {
		t.Fatal("a second deploy replaced the database password")
	}
}

func TestApplyFailingOnImageLeavesNoDatabaseSecrets(t *testing.T) {
	state, cluster := reconcilerStore(t)
	box, err := security.NewSecretBox(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	s := newService(state, cluster, nil, nil, box, nil, time.Now)
	s.imageResolver = imageResolverFunc(func(context.Context, string, string, []byte) (string, error) {
		return "", images.ErrNotFound
	})
	manifest := newTestManifest("shop", map[string]config.Service{
		"main": {Image: "postgres:17", TCP: &config.TCP{Port: 5432}, SecretEnv: map[string]string{"POSTGRES_PASSWORD": "main-password"},
			Database: &config.Database{Engine: config.EnginePostgres, Version: "17"}},
	})
	if _, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{
		CanonicalManifestJson: mustCanonicalConfig(t, manifest), IdempotencyKey: "database-image-fails",
	})); err == nil {
		t.Fatal("deploy with an unresolvable image succeeded")
	}
	if _, err := state.ResolveApplicationSecret(ctx, box, "shop", "main-password"); !errors.Is(err, store.ErrSecretNotFound) {
		t.Fatalf("main-password after a failed deploy: %v, want not found", err)
	}
}
