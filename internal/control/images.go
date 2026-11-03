package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
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
	cache := make(map[string]string)
	resolve := func(path, image string) (string, error) {
		key := image
		if named, err := reference.ParseNormalizedNamed(image); err == nil {
			key = reference.TagNameOnly(named).String()
		}
		if pinned, ok := cache[key]; ok {
			return pinned, nil
		}
		pinned, err := s.resolveImage(ctx, path, image)
		if err != nil {
			return "", err
		}
		cache[key] = pinned
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

// resolveImage pins one authored image reference to the digest its registry
// reports, using stored registry credentials. path names the reference in
// errors.
func (s *service) resolveImage(ctx context.Context, path, image string) (string, error) {
	resolver := s.imageResolver
	if resolver == nil {
		resolver = images.Resolver{}
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: invalid image reference", path))
	}
	if _, ok := named.(reference.Digested); ok {
		return image, nil
	}
	normalized := reference.TagNameOnly(named).String()
	credential, err := s.state.ResolveRegistryCredential(ctx, s.secretBox, reference.Domain(named))
	if errors.Is(err, store.ErrRegistryCredentialNotFound) {
		credential = model.ResolvedRegistryCredential{}
	} else if err != nil {
		return "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: registry credentials are unavailable", path))
	}
	defer func() { clear(credential.Secret) }()
	// A deployer's image reference must not make the controller probe hosts
	// on its own network, unless an administrator logged in to that registry.
	// Administrators and the controller's own auto-update loop, which only
	// follows images an earlier deploy accepted, resolve any registry.
	if principal, _ := ctx.Value(principalKey{}).(authenticatedPrincipal); credential.Username == "" && principal.application != "" {
		ctx = images.WithPublicRegistriesOnly(ctx)
	}
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
		case errors.Is(err, images.ErrPrivate):
			code, reason = connect.CodePermissionDenied, images.ErrPrivate
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
	return pinned, nil
}

// requireImageAccess keeps a deployer limited to one application from
// running another application's private images. Registry credentials are
// stored per registry host and nodes pull with them, so a deployer could
// otherwise name any repository an administrator's login can read. A
// repository counts as another application's when only other applications
// deploy it right now; such an image must then be public. Images uploaded
// with orch deploy must have been uploaded for this application.
// Administrators deploy anything, which is also how two applications come
// to share a private image.
func (s *service) requireImageAccess(ctx context.Context, manifest *config.Manifest) error {
	principal, _ := ctx.Value(principalKey{}).(authenticatedPrincipal)
	if principal.application == "" {
		return nil
	}
	var credentialHosts map[string]bool
	var users map[string][]string
	for _, item := range manifestImages(manifest) {
		named, err := reference.ParseNormalizedNamed(item.image)
		if err != nil {
			// resolveImages reports it.
			continue
		}
		if digested, ok := named.(reference.Digested); ok && s.uploads != nil && s.uploads.Has(ctx, digested.Digest().String()) {
			owned, err := s.uploads.UploadedBy(principal.application, digested.Digest().String())
			if err != nil {
				return connect.NewError(connect.CodeInternal, errors.New("read the image store"))
			}
			if !owned {
				return connect.NewError(connect.CodePermissionDenied,
					fmt.Errorf("%s: this image was uploaded for another application; deploy it from your local engine so orch deploy uploads it for %s",
						item.path, principal.application))
			}
			continue
		}
		if credentialHosts == nil {
			if credentialHosts, err = s.registryCredentialHosts(ctx); err != nil {
				return err
			}
		}
		if !credentialHosts[reference.Domain(named)] {
			// Nothing to borrow: without a login every pull is anonymous.
			continue
		}
		if users == nil {
			if users, err = s.repositoryUsers(ctx); err != nil {
				return err
			}
		}
		repository := reference.TrimNamed(named).String()
		using := users[repository]
		if len(using) == 0 || slices.Contains(using, principal.application) || s.publicImage(ctx, named) {
			continue
		}
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("%s: %s is a private image of application %s; ask an administrator to deploy it for %s",
				item.path, repository, using[0], principal.application))
	}
	return nil
}

type manifestImage struct{ path, image string }

func manifestImages(manifest *config.Manifest) []manifestImage {
	var result []manifestImage
	for _, name := range slices.Sorted(maps.Keys(manifest.Services)) {
		result = append(result, manifestImage{"services." + name + ".image", manifest.Services[name].Image})
	}
	for _, name := range slices.Sorted(maps.Keys(manifest.CronJobs)) {
		result = append(result, manifestImage{"cronJobs." + name + ".image", manifest.CronJobs[name].Image})
	}
	return result
}

func (s *service) registryCredentialHosts(ctx context.Context) (map[string]bool, error) {
	credentials, err := s.state.RegistryCredentials(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read registry credentials"))
	}
	hosts := make(map[string]bool, len(credentials))
	for _, credential := range credentials {
		hosts[credential.Host] = true
	}
	return hosts, nil
}

// repositoryUsers maps each image repository to the applications whose
// current revision runs it.
func (s *service) repositoryUsers(ctx context.Context) (map[string][]string, error) {
	applications, err := s.state.Applications(ctx, false)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read application state"))
	}
	users := make(map[string][]string)
	for _, application := range applications {
		var manifest config.Manifest
		if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
			continue
		}
		for _, item := range manifestImages(&manifest) {
			named, err := reference.ParseNormalizedNamed(item.image)
			if err != nil {
				continue
			}
			repository := reference.TrimNamed(named).String()
			if !slices.Contains(users[repository], application.Name) {
				users[repository] = append(users[repository], application.Name)
			}
		}
	}
	return users, nil
}

// publicImage reports whether the registry serves the image without
// credentials, digest references included.
func (s *service) publicImage(ctx context.Context, named reference.Named) bool {
	resolver := s.imageResolver
	if resolver == nil {
		resolver = images.Resolver{}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ctx = images.WithDigestCheck(images.WithPublicRegistriesOnly(ctx))
	_, err := resolver.Resolve(ctx, reference.TagNameOnly(named).String(), "", nil)
	return err == nil
}
