package control

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/images"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/store"
	"connectrpc.com/connect"
	"github.com/distribution/reference"
)

// ImageResolver allows controller embedders to supply a registry trust policy.
// Implementations must return a digest-qualified reference to the same repository.
type ImageResolver interface {
	Resolve(context.Context, string, string, []byte) (string, error)
}

func (s *service) resolveImages(ctx context.Context, manifest *config.Manifest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resolver := s.imageResolver
	if resolver == nil {
		resolver = images.Resolver{}
	}
	cache := make(map[string]string)
	resolve := func(path, image string) (string, error) {
		named, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: invalid image reference", path))
		}
		if _, ok := named.(reference.Digested); ok {
			return image, nil
		}
		normalized := reference.TagNameOnly(named).String()
		if pinned, ok := cache[normalized]; ok {
			return pinned, nil
		}
		credential, err := s.state.ResolveRegistryCredential(ctx, s.secretBox, reference.Domain(named))
		if errors.Is(err, store.ErrRegistryCredentialNotFound) {
			credential = model.ResolvedRegistryCredential{}
		} else if err != nil {
			return "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: registry credentials are unavailable", path))
		}
		defer func() { clear(credential.Secret) }()
		pinned, err := resolver.Resolve(ctx, normalized, credential.Username, credential.Secret)
		if err != nil {
			code, reason := connect.CodeUnavailable, images.ErrUnavailable
			switch {
			case errors.Is(err, context.Canceled):
				code, reason = connect.CodeCanceled, context.Canceled
			case errors.Is(err, context.DeadlineExceeded):
				code, reason = connect.CodeDeadlineExceeded, context.DeadlineExceeded
			case errors.Is(err, images.ErrNotFound):
				code, reason = connect.CodeNotFound, images.ErrNotFound
			case errors.Is(err, images.ErrUnauthorized):
				code, reason = connect.CodeFailedPrecondition, images.ErrUnauthorized
			case errors.Is(err, images.ErrPlatform):
				code, reason = connect.CodeFailedPrecondition, images.ErrPlatform
			case errors.Is(err, images.ErrInvalid):
				code, reason = connect.CodeFailedPrecondition, images.ErrInvalid
			}
			return "", connect.NewError(code, fmt.Errorf("%s: %w", path, reason))
		}
		resolved, err := reference.ParseNormalizedNamed(pinned)
		if err != nil || reference.TrimNamed(resolved).String() != reference.TrimNamed(named).String() {
			return "", connect.NewError(connect.CodeInternal, errors.New("resolver returned a different image repository"))
		}
		if _, ok := resolved.(reference.Digested); !ok {
			return "", connect.NewError(connect.CodeInternal, errors.New("resolver returned an unpinned image"))
		}
		cache[normalized] = pinned
		return pinned, nil
	}
	names := make([]string, 0, len(manifest.Services))
	for name := range manifest.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		item := manifest.Services[name]
		pinned, err := resolve("services."+name+".image", item.Image)
		if err != nil {
			return nil, err
		}
		item.Image = pinned
		manifest.Services[name] = item
	}
	names = names[:0]
	for name := range manifest.CronJobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		item := manifest.CronJobs[name]
		pinned, err := resolve("cronJobs."+name+".image", item.Image)
		if err != nil {
			return nil, err
		}
		item.Image = pinned
		manifest.CronJobs[name] = item
	}
	canonical, err := config.CanonicalJSON(manifest)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("encode resolved manifest"))
	}
	if len(canonical) > maxManifestBytes {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("resolved manifest exceeds size limit"))
	}
	return canonical, nil
}
