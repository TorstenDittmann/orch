package staticsite

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	nodebundle "github.com/TorstenDittmann/orch/packaging/node"
)

const maxCaddyArchiveBytes = 256 << 20

// caddyRelease is the pinned Caddy release for one architecture, from the
// node bundle that ingress installs, so sites run the same reviewed server.
type caddyRelease struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func pinnedCaddy(architecture string) (caddyRelease, error) {
	data, err := nodebundle.Files.ReadFile("artifacts.json")
	if err != nil {
		return caddyRelease{}, err
	}
	var bundles map[string][]caddyRelease
	if err := json.Unmarshal(data, &bundles); err != nil {
		return caddyRelease{}, err
	}
	for _, artifact := range bundles[architecture] {
		if strings.HasPrefix(artifact.Name, "caddy_") {
			return artifact, nil
		}
	}
	return caddyRelease{}, fmt.Errorf("static sites cannot run on linux/%s nodes", architecture)
}

// openCaddy returns the Caddy executable for architecture and its size. The
// release archive is downloaded once into the user's cache directory and
// checked against its pinned SHA-256 on every use.
func openCaddy(ctx context.Context, architecture string) (io.ReadCloser, int64, error) {
	release, err := pinnedCaddy(architecture)
	if err != nil {
		return nil, 0, err
	}
	archive, err := cachedRelease(ctx, release)
	if err != nil {
		return nil, 0, err
	}
	zipped, err := gzip.NewReader(archive)
	if err != nil {
		archive.Close()
		return nil, 0, fmt.Errorf("read %s: %w", release.Name, err)
	}
	reader := tar.NewReader(zipped)
	for {
		header, err := reader.Next()
		if err != nil {
			archive.Close()
			if errors.Is(err, io.EOF) {
				return nil, 0, fmt.Errorf("%s has no caddy executable", release.Name)
			}
			return nil, 0, fmt.Errorf("read %s: %w", release.Name, err)
		}
		if header.Typeflag == tar.TypeReg && strings.TrimPrefix(header.Name, "./") == "caddy" {
			return struct {
				io.Reader
				io.Closer
			}{reader, archive}, header.Size, nil
		}
	}
}

func cachedRelease(ctx context.Context, release caddyRelease) (*os.File, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	dir := filepath.Join(cache, "orch", "artifacts")
	name := filepath.Join(dir, release.Name)
	if file, err := os.Open(name); err == nil {
		if verify(file, release.SHA256) == nil {
			return file, nil
		}
		file.Close()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := download(ctx, release, dir, name); err != nil {
		return nil, err
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	if err := verify(file, release.SHA256); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func download(ctx context.Context, release caddyRelease, dir, name string) error {
	parsed, err := url.Parse(release.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
		return fmt.Errorf("invalid pinned URL for %s", release.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("download %s: %w", release.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s returned HTTP %d", release.Name, response.StatusCode)
	}
	temporary, err := os.CreateTemp(dir, release.Name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, maxCaddyArchiveBytes+1))
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", release.Name, err)
	}
	if written > maxCaddyArchiveBytes || hex.EncodeToString(hash.Sum(nil)) != release.SHA256 {
		return fmt.Errorf("download %s: checksum mismatch", release.Name)
	}
	return os.Rename(temporary.Name(), name)
}

// verify checks file against its pinned digest and rewinds it.
func verify(file *os.File, want string) error {
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxCaddyArchiveBytes+1)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != want {
		return fmt.Errorf("%s: checksum mismatch", file.Name())
	}
	_, err := file.Seek(0, io.SeekStart)
	return err
}
