package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/staticsite"
)

// buildStaticSite is replaced in tests to avoid downloading Caddy.
var buildStaticSite = staticsite.Build

// uploadStaticSites builds the image of every static service, uploads it
// unless the controller has it from an earlier deploy, and pins the service
// to it.
func uploadStaticSites(ctx context.Context, remote imageUploader, manifest *config.Manifest, progress io.Writer) error {
	var names []string
	for name, service := range manifest.Services {
		if service.Static != nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	architectures, err := nodeArchitectures(ctx, remote)
	if err != nil {
		return err
	}
	if len(architectures) == 0 {
		architectures = staticsite.Architectures
	}
	workDir, err := os.MkdirTemp("", "orch-static-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	for _, name := range names {
		service := manifest.Services[name]
		dir := filepath.Join(workDir, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		// A health path other than the server's own must be a page of the
		// site, or no replica ever becomes ready.
		if path := service.HTTP.HealthPath; path != "" && path != staticsite.HealthPath && !service.Static.SPA && !staticsite.Serves(*service.Static, path) {
			return fmt.Errorf("static site %s: healthPath %s is not a file in %s; remove it to use the built-in %s", name, path, service.Static.Dir, staticsite.HealthPath)
		}
		built, err := buildStaticSite(ctx, *service.Static, service.HTTP.Port, architectures, dir)
		if err != nil {
			return fmt.Errorf("build static site %s: %w", name, err)
		}
		image := config.StaticImage(manifest.Name, name)
		found, err := remote.LookupImage(ctx, "", built.ID)
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return errors.New("this controller cannot store uploaded images; upgrade it to deploy static sites")
		}
		if err != nil {
			return fmt.Errorf("check static site %s: %w", name, err)
		}
		digest := found.GetDigest()
		if digest == "" {
			fmt.Fprintf(progress, "Uploading static site %s (%s)\n", name, formatImageSize(built.Size))
			archive, err := os.Open(built.Path)
			if err != nil {
				return err
			}
			uploaded, err := remote.UploadImage(ctx, image, built.ID, archive)
			archive.Close()
			if err != nil {
				return fmt.Errorf("upload static site %s: %w", name, err)
			}
			digest = uploaded.GetDigest()
		} else {
			fmt.Fprintf(progress, "Using static site %s, uploaded before\n", name)
		}
		if digest != built.ID {
			return fmt.Errorf("upload static site %s: the controller stored it as %q, not %s", name, digest, built.ID)
		}
		service.Image = image + "@" + digest
		manifest.Services[name] = service
	}
	return nil
}
