package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRegistryResolution(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch       string
		index, auth, corrupt bool
		status               int
		want                 error
	}{
		{name: "single amd64", os: "linux", arch: "amd64"},
		{name: "arm64 independent of controller", os: "linux", arch: "arm64"},
		{name: "multi-platform index", os: "linux", arch: "amd64", index: true},
		{name: "private", os: "linux", arch: "amd64", auth: true},
		{name: "corrupt metadata", os: "linux", arch: "amd64", corrupt: true, want: ErrInvalid},
		{name: "missing", status: 404, want: ErrNotFound},
		{name: "denied", status: 401, want: ErrUnauthorized},
		{name: "unavailable", status: 503, want: ErrUnavailable},
		{name: "windows", os: "windows", arch: "amd64", want: ErrPlatform},
		{name: "unsupported architecture", os: "linux", arch: "s390x", want: ErrPlatform},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blobs := map[string][]byte{}
			descriptor := func(media string, value any) ocispec.Descriptor {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				d := ocispec.Descriptor{MediaType: media, Digest: digest.FromBytes(data), Size: int64(len(data))}
				blobs[d.Digest.String()] = data
				return d
			}
			config := descriptor(ocispec.MediaTypeImageConfig, ocispec.Image{Platform: ocispec.Platform{OS: tc.os, Architecture: tc.arch}})
			target := descriptor(ocispec.MediaTypeImageManifest, ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, Config: config})
			if tc.index {
				target.Platform = &ocispec.Platform{OS: tc.os, Architecture: tc.arch}
				target = descriptor(ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{target}})
			}
			var authenticated atomic.Bool
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, pass, ok := r.BasicAuth()
				if tc.status == 401 || (tc.auth && (!ok || user != "robot" || pass != "private-password")) {
					w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
					w.WriteHeader(401)
					return
				}
				if tc.auth {
					authenticated.Store(true)
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				if key == "latest" {
					key = target.Digest.String()
				}
				data, ok := blobs[key]
				if !ok {
					w.WriteHeader(404)
					return
				}
				media := target.MediaType
				if key == config.Digest.String() {
					media = config.MediaType
				}
				w.Header().Set("Content-Type", media)
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				w.Header().Set("Docker-Content-Digest", key)
				if r.Method != "HEAD" {
					if tc.corrupt {
						data = append([]byte(nil), data...)
						data[0] = '!'
					}
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "https://")
			got, err := (Resolver{Client: server.Client()}).Resolve(context.Background(), host+"/test:latest", "robot", []byte("private-password"))
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v; want %v", err, tc.want)
				}
				if strings.Contains(err.Error(), "private-password") {
					t.Fatal("credential exposed")
				}
				return
			}
			if err != nil || got != host+"/test@"+target.Digest.String() {
				t.Fatalf("resolved = %s, %v", got, err)
			}
			if tc.auth && !authenticated.Load() {
				t.Fatal("credentials were not used")
			}
		})
	}
}

func TestDigestDoesNotContactRegistry(t *testing.T) {
	image := "registry.invalid/test@sha256:" + strings.Repeat("a", 64)
	got, err := (Resolver{}).Resolve(context.Background(), image, "", nil)
	if err != nil || got != image {
		t.Fatalf("%s %v", got, err)
	}
}

func TestRejectPlainHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	request, _ := http.NewRequest("GET", server.URL, nil)
	_, err := (httpsTransport{base: http.DefaultTransport}).RoundTrip(request)
	if !errors.Is(err, ErrUnavailable) || requests.Load() != 0 {
		t.Fatalf("plaintext request: %v", err)
	}
}

func TestResolveRefusesPrivateRegistriesWhenAskedTo(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(name, "")
	}
	ctx := WithPublicRegistriesOnly(context.Background())
	for _, image := range []string{"127.0.0.1:5000/test:latest", "10.1.2.3:5000/test:latest", "localhost:5000/test:latest", "169.254.169.254/test:latest"} {
		if _, err := (Resolver{}).Resolve(ctx, image, "", nil); !errors.Is(err, ErrPrivate) {
			t.Errorf("%s: err = %v, want ErrPrivate", image, err)
		}
	}
	for _, address := range []string{"100.64.0.1", "192.168.1.1", "::1", "fd00::1", "0.0.0.0"} {
		if publicAddress(netip.MustParseAddr(address)) {
			t.Errorf("%s counted as public", address)
		}
	}
	if !publicAddress(netip.MustParseAddr("140.82.112.3")) {
		t.Error("a public address was refused")
	}
}

func TestResolveChecksHostsTheProxySkips(t *testing.T) {
	for _, name := range []string{"https_proxy", "HTTP_PROXY", "http_proxy", "no_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "10.0.0.0/8")
	ctx := WithPublicRegistriesOnly(context.Background())
	if _, err := (Resolver{}).Resolve(ctx, "10.1.2.3:5000/test:latest", "", nil); !errors.Is(err, ErrPrivate) {
		t.Fatalf("registry dialed around the proxy: err = %v, want ErrPrivate", err)
	}
	if !proxyAddress("proxy.example:3128") || proxyAddress("10.1.2.3:5000") {
		t.Fatal("proxyAddress does not tell the proxy from a registry")
	}
}
