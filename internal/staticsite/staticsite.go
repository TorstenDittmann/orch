// Package staticsite builds the image orch deploys for a service with a
// static block: the service's folder of files, served by the same pinned
// Caddy release that ingress nodes run. The image is built in Go, so
// deploying a site needs no container engine on this machine.
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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// HealthPath answers 200 in every site, so readiness does not depend on the
// site's own pages.
const HealthPath = "/.orch/health"

// Architectures are the node architectures a site image can be built for.
var Architectures = []string{"amd64", "arm64"}

// Image is a built site image in an OCI layout archive.
type Image struct {
	// ID is the digest of the image index. It only changes when the site,
	// its settings or the server change, so an unchanged site is not
	// uploaded again.
	ID   string
	Path string
	Size int64
}

// caddyBinary opens the Caddy executable for a Linux architecture. Tests
// replace it to avoid the download.
var caddyBinary = openCaddy

// Build packs site into an image that serves it on port for each of
// architectures, writing the archive into workDir.
func Build(ctx context.Context, site config.Static, port int, architectures []string, workDir string) (Image, error) {
	info, err := os.Stat(site.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Image{}, fmt.Errorf("static folder %s does not exist; build the site first", site.Dir)
	}
	if err != nil {
		return Image{}, err
	}
	if !info.IsDir() {
		return Image{}, fmt.Errorf("static folder %s is not a folder", site.Dir)
	}
	if len(architectures) == 0 {
		return Image{}, errors.New("no architectures to build the site for")
	}
	blobs := map[digest.Digest]string{}
	siteLayer, siteDiffID, err := writeLayer(workDir, blobs, func(writer *tar.Writer) error {
		return writeSite(writer, site, port)
	})
	if err != nil {
		return Image{}, err
	}
	var manifests []ocispec.Descriptor
	for _, architecture := range architectures {
		caddyLayer, caddyDiffID, err := writeLayer(workDir, blobs, func(writer *tar.Writer) error {
			return writeCaddy(ctx, writer, architecture)
		})
		if err != nil {
			return Image{}, err
		}
		platform := ocispec.Platform{OS: "linux", Architecture: architecture}
		imageConfig := ocispec.Image{
			Platform: platform,
			Config: ocispec.ImageConfig{
				User:         "65534:65534",
				Env:          []string{"PATH=/usr/bin", "HOME=/tmp", "XDG_CONFIG_HOME=/tmp", "XDG_DATA_HOME=/tmp"},
				Entrypoint:   []string{"/usr/bin/caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"},
				WorkingDir:   "/srv",
				ExposedPorts: map[string]struct{}{fmt.Sprintf("%d/tcp", port): {}},
			},
			RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{caddyDiffID, siteDiffID}},
		}
		configDescriptor, err := writeJSON(workDir, blobs, ocispec.MediaTypeImageConfig, imageConfig)
		if err != nil {
			return Image{}, err
		}
		manifest, err := writeJSON(workDir, blobs, ocispec.MediaTypeImageManifest, ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    configDescriptor,
			Layers:    []ocispec.Descriptor{caddyLayer, siteLayer},
		})
		if err != nil {
			return Image{}, err
		}
		manifest.Platform = &platform
		manifests = append(manifests, manifest)
	}
	index, err := writeJSON(workDir, blobs, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	})
	if err != nil {
		return Image{}, err
	}
	archivePath := filepath.Join(workDir, "site.tar")
	if err := writeArchive(archivePath, blobs, index); err != nil {
		return Image{}, err
	}
	stat, err := os.Stat(archivePath)
	if err != nil {
		return Image{}, err
	}
	return Image{ID: index.Digest.String(), Path: archivePath, Size: stat.Size()}, nil
}

// Caddyfile is the server configuration of a site served on port.
// notFoundPage serves the site's /404.html for unknown paths.
func Caddyfile(site config.Static, port int, notFoundPage bool) string {
	var out strings.Builder
	out.WriteString("{\n\tadmin off\n\tpersist_config off\n\tauto_https off\n}\n\n")
	fmt.Fprintf(&out, ":%d {\n\troot * /srv\n\tencode zstd gzip\n", port)
	fmt.Fprintf(&out, "\thandle %s {\n\t\trespond 200\n\t}\n", HealthPath)
	out.WriteString("\thandle {\n")
	if site.SPA {
		out.WriteString("\t\ttry_files {path} {path}/ /index.html\n")
	}
	out.WriteString("\t\tfile_server\n\t}\n")
	if !site.SPA && notFoundPage {
		out.WriteString("\thandle_errors 404 {\n\t\trewrite * /404.html\n\t\tfile_server\n\t}\n")
	}
	out.WriteString("}\n")
	return out.String()
}

// writeSite writes the site's files below /srv, the server configuration,
// and a writable /tmp for Caddy's state. Every entry carries the newest
// modification time in the folder, so the layer only changes with the site
// and browsers still see a new Last-Modified after a change.
func writeSite(writer *tar.Writer, site config.Static, port int) error {
	type entry struct {
		name   string
		source string
		dir    bool
	}
	var entries []entry
	var newest time.Time
	root, err := filepath.EvalSymlinks(site.Dir)
	if err != nil {
		return err
	}
	err = filepath.WalkDir(site.Dir, func(file string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(site.Dir, file)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		// Hidden files such as .env or .git are never published.
		if name := item.Name(); strings.HasPrefix(name, ".") && name != ".well-known" {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		if item.Type()&fs.ModeSymlink != 0 {
			if info.IsDir() {
				return fmt.Errorf("static folder: %s links to a folder, which is not supported", relative)
			}
			if err := checkLink(root, file, relative); err != nil {
				return err
			}
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("static folder: %s is not a regular file", relative)
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		entries = append(entries, entry{name: "srv/" + filepath.ToSlash(relative), source: file, dir: info.IsDir()})
		return nil
	})
	if err != nil {
		return err
	}
	modified := newest.UTC().Truncate(time.Second)
	if newest.IsZero() {
		modified = time.Unix(0, 0).UTC()
	}
	notFoundPage := false
	for _, item := range entries {
		notFoundPage = notFoundPage || item.name == "srv/404.html" && !item.dir
	}
	caddyfile := []byte(Caddyfile(site, port, notFoundPage))
	for _, dir := range []string{"etc/", "etc/caddy/", "srv/"} {
		if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755, ModTime: modified}); err != nil {
			return err
		}
	}
	if err := writeFile(writer, "etc/caddy/Caddyfile", 0o644, modified, caddyfile); err != nil {
		return err
	}
	for _, item := range entries {
		if item.dir {
			if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: item.name + "/", Mode: 0o755, ModTime: modified}); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(writer, item.name, item.source, modified); err != nil {
			return err
		}
	}
	return writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "tmp/", Mode: 0o1777, ModTime: modified})
}

// checkLink allows a link to a file only when the file is a visible part of
// the folder, so a link cannot publish a hidden file or one outside it.
func checkLink(root, file, relative string) error {
	target, err := filepath.EvalSymlinks(file)
	if err != nil {
		return err
	}
	inside, err := filepath.Rel(root, target)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
		return fmt.Errorf("static folder: %s links outside the folder, which is not supported", relative)
	}
	for _, part := range strings.Split(filepath.ToSlash(inside), "/") {
		if strings.HasPrefix(part, ".") && part != ".well-known" {
			return fmt.Errorf("static folder: %s links to a hidden file, which is not published", relative)
		}
	}
	return nil
}

// Serves reports whether the folder answers path with a file, so a health
// path other than HealthPath can become ready.
func Serves(site config.Static, path string) bool {
	for _, part := range strings.Split(path, "/") {
		if strings.HasPrefix(part, ".") && part != ".well-known" {
			return false
		}
	}
	name := filepath.Join(site.Dir, filepath.FromSlash(strings.TrimPrefix(path, "/")))
	if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
		return true
	}
	info, err := os.Stat(filepath.Join(name, "index.html"))
	return err == nil && info.Mode().IsRegular()
}

func copyFile(writer *tar.Writer, name, source string, modified time.Time) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: info.Size(), ModTime: modified}); err != nil {
		return err
	}
	if _, err := io.CopyN(writer, file, info.Size()); err != nil {
		return fmt.Errorf("read %s: %w", source, err)
	}
	return nil
}

func writeFile(writer *tar.Writer, name string, mode int64, modified time.Time, data []byte) error {
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)), ModTime: modified}); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

// writeCaddy writes the Caddy executable for architecture as /usr/bin/caddy.
func writeCaddy(ctx context.Context, writer *tar.Writer, architecture string) error {
	binary, size, err := caddyBinary(ctx, architecture)
	if err != nil {
		return err
	}
	defer binary.Close()
	epoch := time.Unix(0, 0).UTC()
	for _, dir := range []string{"usr/", "usr/bin/"} {
		if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755, ModTime: epoch}); err != nil {
			return err
		}
	}
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "usr/bin/caddy", Mode: 0o755, Size: size, ModTime: epoch}); err != nil {
		return err
	}
	_, err = io.CopyN(writer, binary, size)
	return err
}

// writeLayer writes a gzip-compressed layer blob and returns its descriptor
// and the digest of its uncompressed tar.
func writeLayer(workDir string, blobs map[digest.Digest]string, fill func(*tar.Writer) error) (ocispec.Descriptor, digest.Digest, error) {
	file, err := os.CreateTemp(workDir, "layer-")
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	defer file.Close()
	compressed := sha256.New()
	counter := &countingWriter{writer: io.MultiWriter(file, compressed)}
	zipped := gzip.NewWriter(counter)
	uncompressed := sha256.New()
	writer := tar.NewWriter(io.MultiWriter(zipped, uncompressed))
	if err := fill(writer); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	if err := writer.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	if err := zipped.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	if err := file.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	value := digest.NewDigestFromBytes(digest.SHA256, compressed.Sum(nil))
	blobs[value] = file.Name()
	descriptor := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: value, Size: counter.size}
	return descriptor, digest.NewDigestFromBytes(digest.SHA256, uncompressed.Sum(nil)), nil
}

func writeJSON(workDir string, blobs map[digest.Digest]string, mediaType string, value any) (ocispec.Descriptor, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	sum := sha256.Sum256(data)
	name := filepath.Join(workDir, hex.EncodeToString(sum[:])+".json")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return ocispec.Descriptor{}, err
	}
	blob := digest.NewDigestFromEncoded(digest.SHA256, hex.EncodeToString(sum[:]))
	blobs[blob] = name
	return ocispec.Descriptor{MediaType: mediaType, Digest: blob, Size: int64(len(data))}, nil
}

// writeArchive writes an OCI image layout tar whose index.json names the
// site's image index.
func writeArchive(name string, blobs map[digest.Digest]string, index ocispec.Descriptor) error {
	file, err := os.Create(name)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := tar.NewWriter(file)
	epoch := time.Unix(0, 0).UTC()
	layout, _ := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err := writeFile(writer, ocispec.ImageLayoutFile, 0o644, epoch, layout); err != nil {
		return err
	}
	root, _ := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{index},
	})
	if err := writeFile(writer, ocispec.ImageIndexFile, 0o644, epoch, root); err != nil {
		return err
	}
	values := make([]string, 0, len(blobs))
	for value := range blobs {
		values = append(values, value.String())
	}
	sort.Strings(values)
	for _, value := range values {
		parsed := digest.Digest(value)
		blob, err := os.Open(blobs[parsed])
		if err != nil {
			return err
		}
		info, err := blob.Stat()
		if err == nil {
			err = writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: path.Join("blobs", parsed.Algorithm().String(), parsed.Encoded()), Mode: 0o644, Size: info.Size(), ModTime: epoch})
		}
		if err == nil {
			_, err = io.Copy(writer, blob)
		}
		blob.Close()
		if err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return file.Close()
}

type countingWriter struct {
	writer io.Writer
	size   int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.size += int64(n)
	return n, err
}
