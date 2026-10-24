// Package imagetest builds image archives for tests.
package imagetest

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// DockerArchive returns what docker image save writes for a one-layer image
// tagged tag for linux/architecture, and that layer's tar.
func DockerArchive(t testing.TB, tag, architecture, contents string) (archive, layer []byte) {
	t.Helper()
	var layerBuffer bytes.Buffer
	layerWriter := tar.NewWriter(&layerBuffer)
	if err := layerWriter.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := layerWriter.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := layerWriter.Close(); err != nil {
		t.Fatal(err)
	}
	layer = layerBuffer.Bytes()
	layerSum := sha256.Sum256(layer)
	layerHex := hex.EncodeToString(layerSum[:])
	config, err := json.Marshal(map[string]any{
		"architecture": architecture, "os": "linux", "config": map[string]any{},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + layerHex}},
	})
	if err != nil {
		t.Fatal(err)
	}
	configSum := sha256.Sum256(config)
	configName := hex.EncodeToString(configSum[:]) + ".json"
	manifest, err := json.Marshal([]map[string]any{{
		"Config": configName, "RepoTags": []string{tag}, "Layers": []string{layerHex + "/layer.tar"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, file := range []struct {
		name string
		data []byte
	}{{configName, config}, {layerHex + "/layer.tar", layer}, {"manifest.json", manifest}} {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), layer
}
