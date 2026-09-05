package host

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const maxArtifactBytes = int64(256 << 20)

func download(ctx context.Context, a Artifact, destination string) error {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return errors.New("invalid pinned artifact URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: artifactRedirect}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s failed", a.Name)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s returned HTTP %d", a.Name, response.StatusCode)
	}
	if response.ContentLength > maxArtifactBytes {
		return errors.New("artifact exceeds size limit")
	}
	return saveVerified(response.Body, destination, a.SHA256)
}
func saveVerified(input io.Reader, destination, digest string) (finalErr error) {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		file.Close()
		if finalErr != nil {
			os.Remove(destination)
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(input, maxArtifactBytes+1))
	if err != nil {
		return err
	}
	if n > maxArtifactBytes {
		return errors.New("artifact exceeds size limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("artifact checksum mismatch")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}
func extract(a Artifact, source, destination string) error {
	if err := verifyHash(source, a.SHA256); err != nil {
		return err
	}
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	if a.Archive == "raw" {
		if len(a.Files) != 1 {
			return errors.New("raw artifact must select one executable")
		}
		for _, name := range a.Files {
			return extractFile(f, destination, name, maxArtifactBytes)
		}
		return errors.New("raw artifact has no file")
	}
	if a.Archive != "tar.gz" {
		return errors.New("unsupported artifact archive type")
	}
	zipped, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zipped.Close()
	archive := tar.NewReader(zipped)
	found := map[string]bool{}
	var total int64
	headers := 0
	for {
		h, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		headers++
		if headers > 1024 {
			return errors.New("artifact has too many entries")
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "." || name == "" {
			if h.Typeflag == tar.TypeDir {
				continue
			}
		}
		if path.IsAbs(name) || path.Clean(name) != strings.TrimSuffix(name, "/") || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return errors.New("unsafe artifact path")
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return errors.New("artifact contains link or special file")
		}
		total += h.Size
		if h.Size < 0 || total > 512<<20 {
			return errors.New("expanded artifact exceeds limit")
		}
		if target, ok := a.Files[name]; ok {
			if found[name] {
				return errors.New("duplicate artifact entry")
			}
			found[name] = true
			if err := extractFile(archive, destination, target, h.Size); err != nil {
				return err
			}
		} else if !knownExtra(a.Name, name) {
			return fmt.Errorf("unexpected artifact entry %q", name)
		}
	}
	if len(found) != len(a.Files) {
		return errors.New("artifact is missing required files")
	}
	return nil
}
func knownExtra(artifact, name string) bool {
	if strings.HasPrefix(artifact, "containerd-") {
		return name == "bin/ctr" || name == "bin/containerd-shim-runc-v1" || name == "bin/containerd-stress"
	}
	if strings.HasPrefix(artifact, "caddy_") {
		return name == "LICENSE" || name == "README.md"
	}
	if strings.HasPrefix(artifact, "cni-") {
		return strings.Contains(" bandwidth dhcp dummy firewall host-device ipvlan loopback macvlan portmap ptp sbr static tap tuning vlan vrf bridge host-local LICENSE README.md ", " "+name+" ")
	}
	return false
}
func extractFile(input io.Reader, root, name string, size int64) error {
	if path.Clean(name) != name || strings.HasPrefix(name, "../") || path.IsAbs(name) {
		return errors.New("invalid artifact destination")
	}
	dest := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(input, size)); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	executable, err := elf.Open(dest)
	if err != nil {
		return fmt.Errorf("artifact is not an ELF executable: %s", name)
	}
	defer executable.Close()
	if executable.Machine != elf.EM_X86_64 || executable.Class != elf.ELFCLASS64 || (executable.Type != elf.ET_EXEC && executable.Type != elf.ET_DYN) {
		return errors.New("artifact is not an amd64 executable")
	}
	return nil
}
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func verifyHash(path, digest string) error {
	if _, err := privateRegular(path); err != nil {
		return err
	}
	actual, err := hashFile(path)
	if err != nil {
		return err
	}
	if actual != digest {
		return fmt.Errorf("owned file checksum mismatch: %s", path)
	}
	return nil
}

func artifactRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 5 || req.URL.Scheme != "https" || req.URL.User != nil {
		return errors.New("unsafe artifact redirect")
	}
	return nil
}
