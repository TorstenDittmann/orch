package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	agentctl "github.com/TorstenDittmann/orch/internal/agent"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/TorstenDittmann/orch/internal/images/imagetest"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestControllerStoresUploadedImagesAndServesThemToAgents(t *testing.T) {
	address := reserveLoopbackAddress(t)
	root := t.TempDir()
	controllerDir := filepath.Join(root, "controller")
	contextPath := filepath.Join(root, "operator", "context.json")
	if _, err := clusterctl.Bootstrap(context.Background(), clusterctl.BootstrapOptions{
		Name: "test", Server: "https://" + address, DataDir: controllerDir, ContextPath: contextPath,
		Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := Start(context.Background(), Options{DataDir: controllerDir, Listen: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	remote, err := controlclient.New(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.CloseIdleConnections()
	ctx := context.Background()

	localID := "sha256:" + strings.Repeat("b", 64)
	if found, err := remote.LookupImage(ctx, "", localID); err != nil || found.GetDigest() != "" || found.GetInRegistry() {
		t.Fatalf("LookupImage() before upload = %v, %v", found, err)
	}
	// Bigger than one upload message, so the upload streams several.
	archive, layer := imagetest.DockerArchive(t, "myapp:latest", "amd64", strings.Repeat("orch ", 400_000))
	uploaded, err := remote.UploadImage(ctx, "myapp:latest", localID, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	digest := uploaded.GetDigest()
	if !strings.HasPrefix(digest, "sha256:") || len(uploaded.GetPlatforms()) != 1 || uploaded.GetPlatforms()[0] != "linux/amd64" {
		t.Fatalf("UploadImage() = %v", uploaded)
	}
	if found, err := remote.LookupImage(ctx, "", localID); err != nil || found.GetDigest() != digest {
		t.Fatalf("LookupImage() after upload = %v, %v", found, err)
	}

	// Agents pull with containerd's registry client and their certificate.
	agentDir := filepath.Join(root, "agent")
	if _, err := agentctl.Enroll(ctx, agentctl.EnrollOptions{
		Name: "node-1", ContextPath: contextPath, DataDir: agentDir, Now: time.Now, Random: rand.Reader,
	}); err != nil {
		t.Fatal(err)
	}
	credentials, err := agentctl.Load(agentDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	agentClient := &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: credentials.RootCAs, ServerName: credentials.ServerName,
		Certificates: []tls.Certificate{credentials.Certificate},
	}}}
	resolver := docker.NewResolver(docker.ResolverOptions{Hosts: func(string) ([]docker.RegistryHost, error) {
		return []docker.RegistryHost{{Client: agentClient, Host: address, Scheme: "https", Path: "/v2",
			Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve}}, nil
	}})
	reference := "docker.io/library/myapp@" + digest
	_, descriptor, err := resolver.Resolve(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := resolver.Fetcher(ctx, reference)
	if err != nil {
		t.Fatal(err)
	}
	read := func(descriptor ocispec.Descriptor) []byte {
		t.Helper()
		stream, err := fetcher.Fetch(ctx, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		data, err := io.ReadAll(stream)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(read(descriptor), &manifest); err != nil || len(manifest.Layers) != 1 {
		t.Fatalf("manifest = %v, %v", manifest, err)
	}
	if compressed := read(manifest.Layers[0]); len(compressed) == 0 || len(compressed) >= len(layer) {
		t.Fatalf("layer is %d bytes; want a compressed copy of %d bytes", len(compressed), len(layer))
	}

	// Administrator certificates cannot pull.
	administrator, err := clusterctl.LoadAdministratorCredentials(contextPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response, err := tlsHTTPClient(administrator.RootCAs, administrator.Certificate).Get("https://" + address + "/v2/library/myapp/manifests/" + digest)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("administrator pull status = %d, want 401", response.StatusCode)
	}
}

func TestLookupImagePrefersTheRegistry(t *testing.T) {
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	s := newService(state, cluster, nil, nil, nil, nil, time.Now)
	uploads, err := images.OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.uploads = uploads
	s.imageResolver = imageResolverFunc(func(_ context.Context, image, _ string, _ []byte) (string, error) {
		if image == "ghcr.io/acme/api:latest" {
			return "ghcr.io/acme/api@sha256:" + strings.Repeat("1", 64), nil
		}
		return "", images.ErrNotFound
	})
	localID := strings.Repeat("c", 64)
	lookup := func(image string) *orchv1alpha1.LookupImageResponse {
		t.Helper()
		response, err := s.LookupImage(ctx, connect.NewRequest(&orchv1alpha1.LookupImageRequest{Image: image, LocalId: localID}))
		if err != nil {
			t.Fatal(err)
		}
		return response.Msg
	}
	if !lookup("ghcr.io/acme/api:latest").GetInRegistry() {
		t.Fatal("an image its registry has needs no upload")
	}
	if found := lookup("myapp:latest"); found.GetInRegistry() || found.GetDigest() != "" {
		t.Fatalf("an image nowhere = %v", found)
	}
	viewer := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}, application: "hello", readOnly: true})
	if _, err := s.LookupImage(viewer, connect.NewRequest(&orchv1alpha1.LookupImageRequest{LocalId: localID})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("read-only lookup error = %v", err)
	}
}

func TestCollectUploadsKeepsImagesOfRecentDeployments(t *testing.T) {
	state, cluster := reconcilerStore(t)
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	now := time.Now()
	s := newService(state, cluster, nil, nil, nil, nil, time.Now)
	uploads, err := images.OpenStore(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	s.uploads = uploads
	upload := func(tag, id string) string {
		t.Helper()
		archive, _ := imagetest.DockerArchive(t, tag, "amd64", tag)
		digest, _, err := uploads.Import(context.Background(), tag, id, bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	deployed := upload("myapp:latest", strings.Repeat("1", 64))
	unused := upload("old:latest", strings.Repeat("2", 64))
	manifest := newTestManifest("hello", map[string]config.Service{"web": {Image: "docker.io/library/myapp@" + deployed}})
	if _, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{
		CanonicalManifestJson: mustCanonicalConfig(t, manifest), IdempotencyKey: "uploaded-image-deploy",
	})); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if err := s.collectUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !uploads.Has(context.Background(), deployed) || uploads.Has(context.Background(), unused) {
		t.Fatal("collection must keep deployed uploads and remove unused old ones")
	}
}
