package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	configMediaType   = "application/vnd.oci.image.config.v1+json"
	layerMediaType    = "application/vnd.oci.image.layer.v1.tar"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Env        []string `json:"Env"`
		Entrypoint []string `json:"Entrypoint"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
	History []struct {
		CreatedBy string `json:"created_by"`
	} `json:"history"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

func main() {
	var binaryPath, outputPath, repository, architecture string
	flag.StringVar(&binaryPath, "binary", "", "path to the statically linked Linux fixture")
	flag.StringVar(&outputPath, "output", "", "output OCI layout tar path")
	flag.StringVar(&repository, "name", "registry.invalid/orch-e2e", "OCI repository name")
	flag.StringVar(&architecture, "architecture", "", "OCI architecture (amd64 or arm64)")
	flag.Parse()
	if flag.NArg() != 0 || binaryPath == "" || outputPath == "" ||
		(repository == "") || (architecture != "amd64" && architecture != "arm64") {
		fmt.Fprintln(os.Stderr, "binary, output, name, and an amd64 or arm64 architecture are required")
		os.Exit(2)
	}
	if strings.Contains(repository, "@") || strings.Contains(repository, "://") {
		fmt.Fprintln(os.Stderr, "name must be an untagged OCI repository")
		os.Exit(2)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		fail(err)
	}
	layer, err := layerTar(binary)
	if err != nil {
		fail(err)
	}
	layerDescriptor := describe(layerMediaType, layer)

	configuration := imageConfig{Architecture: architecture, OS: "linux"}
	configuration.Config.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	configuration.Config.Entrypoint = []string{"/orch-fixture"}
	configuration.Config.WorkingDir = "/"
	configuration.RootFS.Type = "layers"
	configuration.RootFS.DiffIDs = []string{layerDescriptor.Digest}
	configuration.History = append(configuration.History, struct {
		CreatedBy string `json:"created_by"`
	}{CreatedBy: "orch hermetic containerd fixture"})
	configJSON, err := json.Marshal(configuration)
	if err != nil {
		fail(err)
	}
	configDescriptor := describe(configMediaType, configJSON)

	imageManifest := manifest{
		SchemaVersion: 2, MediaType: manifestMediaType,
		Config: configDescriptor, Layers: []descriptor{layerDescriptor},
	}
	manifestJSON, err := json.Marshal(imageManifest)
	if err != nil {
		fail(err)
	}
	manifestDescriptor := describe(manifestMediaType, manifestJSON)
	reference := repository + "@" + manifestDescriptor.Digest
	manifestDescriptor.Platform = &platform{Architecture: architecture, OS: "linux"}
	manifestDescriptor.Annotations = map[string]string{"org.opencontainers.image.ref.name": reference}
	imageIndex := index{
		SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []descriptor{manifestDescriptor},
	}
	indexJSON, err := json.Marshal(imageIndex)
	if err != nil {
		fail(err)
	}
	layoutJSON := []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n")

	files := []archiveFile{
		{name: blobPath(configDescriptor.Digest), contents: configJSON},
		{name: blobPath(layerDescriptor.Digest), contents: layer},
		{name: blobPath(manifestDescriptor.Digest), contents: manifestJSON},
		{name: "index.json", contents: indexJSON},
		{name: "oci-layout", contents: layoutJSON},
	}
	if err := writeArchive(outputPath, files); err != nil {
		fail(err)
	}
	fmt.Println(reference)
}

type archiveFile struct {
	name     string
	contents []byte
}

func layerTar(binary []byte) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	header := &tar.Header{
		Name: "orch-fixture", Mode: 0o755, Size: int64(len(binary)),
		Uid: 0, Gid: 0, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR,
	}
	if err := writer.WriteHeader(header); err != nil {
		return nil, err
	}
	if _, err := writer.Write(binary); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeArchive(path string, files []archiveFile) (finalErr error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
		if finalErr != nil {
			_ = os.Remove(path)
		}
	}()
	writer := tar.NewWriter(file)
	defer func() {
		if err := writer.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
	}()
	for _, item := range files {
		header := &tar.Header{
			Name: filepath.ToSlash(item.name), Mode: 0o644, Size: int64(len(item.contents)),
			Uid: 0, Gid: 0, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR,
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if _, err := io.Copy(writer, bytes.NewReader(item.contents)); err != nil {
			return err
		}
	}
	return nil
}

func describe(mediaType string, contents []byte) descriptor {
	sum := sha256.Sum256(contents)
	return descriptor{
		MediaType: mediaType, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(contents)),
	}
}

func blobPath(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
