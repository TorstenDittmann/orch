// Package images resolves authored OCI references without selecting the controller's platform.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	remoteerrors "github.com/containerd/containerd/v2/core/remotes/errors"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

var (
	ErrNotFound     = errors.New("image or tag was not found; check the repository and tag")
	ErrUnauthorized = errors.New("registry authentication failed; update credentials with orch registry login")
	ErrUnavailable  = errors.New("registry could not resolve the image; check registry connectivity and TLS")
	ErrPlatform     = errors.New("image has no supported Linux amd64 or arm64 platform")
	ErrInvalid      = errors.New("registry returned invalid or oversized image metadata")
)

// Resolver contacts HTTPS registries only. Client optionally supplies trusted TLS roots.
type Resolver struct{ Client *http.Client }

type httpsTransport struct{ base http.RoundTripper }

func (t httpsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, ErrUnavailable
	}
	return t.base.RoundTrip(r)
}

func (r Resolver) Resolve(ctx context.Context, image, username string, secret []byte) (string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", ErrInvalid
	}
	if _, ok := named.(reference.Digested); ok {
		return image, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Upstream diagnostics may contain registry response bodies or token URLs.
	// Only the sanitized errors below cross this boundary.
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	ctx = log.WithLogger(ctx, logrus.NewEntry(logger))
	client := http.Client{Timeout: 30 * time.Second}
	if r.Client != nil {
		client = *r.Client
	}
	transport := client.Transport
	if transport == nil {
		owned := docker.DefaultHTTPTransport(nil)
		defer owned.CloseIdleConnections()
		transport = owned
	}
	client.Transport = httpsTransport{base: transport}
	// Never forward registry authorization to a redirected host, including subdomains.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return ErrUnavailable
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}
	host := reference.Domain(named)
	authorizer := docker.NewDockerAuthorizer(docker.WithAuthClient(&client), docker.WithAuthCreds(func(requestHost string) (string, string, error) {
		if requestHost == host || (host == "docker.io" && requestHost == "registry-1.docker.io") {
			return username, string(secret), nil
		}
		return "", "", nil
	}))
	resolver := docker.NewResolver(docker.ResolverOptions{Hosts: docker.ConfigureDefaultRegistries(docker.WithClient(&client), docker.WithAuthorizer(authorizer))})
	_, descriptor, err := resolver.Resolve(ctx, reference.TagNameOnly(named).String())
	if err != nil {
		return "", sanitize(ctx, err)
	}
	if descriptor.Digest.Validate() != nil {
		return "", ErrInvalid
	}
	pinned, err := reference.WithDigest(reference.TrimNamed(named), descriptor.Digest)
	if err != nil {
		return "", ErrInvalid
	}
	fetcher, err := resolver.Fetcher(ctx, pinned.String())
	if err != nil {
		return "", sanitize(ctx, err)
	}
	compatible, err := supported(ctx, fetcher, descriptor, 0)
	if err != nil {
		return "", sanitize(ctx, err)
	}
	if !compatible {
		return "", ErrPlatform
	}
	return pinned.String(), nil
}

func sanitize(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var status remoteerrors.ErrUnexpectedStatus
	if errors.As(err, &status) && (status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden) {
		return ErrUnauthorized
	}
	switch {
	case errors.Is(err, ErrInvalid):
		return ErrInvalid
	case errors.Is(err, docker.ErrInvalidAuthorization), errdefs.IsPermissionDenied(err):
		return ErrUnauthorized
	case errdefs.IsNotFound(err):
		return ErrNotFound
	default:
		return ErrUnavailable
	}
}

func readDescriptor(ctx context.Context, fetcher remotes.Fetcher, d ocispec.Descriptor) ([]byte, error) {
	const limit = 4 << 20
	if d.Size < 0 || d.Size > limit || d.Digest.Validate() != nil {
		return nil, ErrInvalid
	}
	stream, err := fetcher.Fetch(ctx, d)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit || int64(len(data)) != d.Size {
		return nil, ErrInvalid
	}
	verifier := d.Digest.Verifier()
	_, _ = verifier.Write(data)
	if !verifier.Verified() {
		return nil, ErrInvalid
	}
	return data, nil
}

func supported(ctx context.Context, fetcher remotes.Fetcher, d ocispec.Descriptor, depth int) (bool, error) {
	if depth > 3 {
		return false, ErrInvalid
	}
	data, err := readDescriptor(ctx, fetcher, d)
	if err != nil {
		return false, err
	}
	switch d.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		var index ocispec.Index
		if json.Unmarshal(data, &index) != nil || len(index.Manifests) > 64 {
			return false, ErrInvalid
		}
		for _, child := range index.Manifests {
			if child.Platform != nil {
				if platformSupported(*child.Platform) {
					return true, nil
				}
				continue
			}
			ok, err := supported(ctx, fetcher, child, depth+1)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case ocispec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		var manifest ocispec.Manifest
		if json.Unmarshal(data, &manifest) != nil {
			return false, ErrInvalid
		}
		config, err := readDescriptor(ctx, fetcher, manifest.Config)
		if err != nil {
			return false, err
		}
		var image ocispec.Image
		if json.Unmarshal(config, &image) != nil {
			return false, ErrInvalid
		}
		return platformSupported(image.Platform), nil
	default:
		return false, fmt.Errorf("%w: unsupported media type", ErrInvalid)
	}
}
func platformSupported(p ocispec.Platform) bool {
	return p.OS == "linux" && (p.Architecture == "amd64" || p.Architecture == "arm64")
}
