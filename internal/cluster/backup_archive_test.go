package cluster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupSetArchiveRoundTrip(t *testing.T) {
	source := t.TempDir()
	files := map[string]string{DatabaseFilename: "database", SecretKeyFilename: "key", "pki/ca.pem": "ca", "pki/ca-key.pem": "ca key",
		"pki/controller.pem": "leaf", "pki/controller-key.pem": "leaf key"}
	if err := os.Mkdir(filepath.Join(source, "pki"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(source, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var archive bytes.Buffer
	if err := ArchiveBackupSet(source, &archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "set")
	if err := ExtractBackupSet(bytes.NewReader(archive.Bytes()), destination); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(got) != contents {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
}

func TestArchiveBackupSetRefusesIncompleteSets(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, SecretKeyFilename), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveBackupSet(source, &bytes.Buffer{}); err == nil {
		t.Fatal("ArchiveBackupSet archived a set without its database")
	}
}

func TestExtractBackupSetRejectsOtherEntries(t *testing.T) {
	for _, name := range []string{"../escape", "pki/../../escape", "notes.txt"} {
		var archive bytes.Buffer
		compressed := gzip.NewWriter(&archive)
		writer := tar.NewWriter(compressed)
		_ = writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
		_, _ = writer.Write([]byte("x"))
		_ = writer.Close()
		_ = compressed.Close()
		destination := filepath.Join(t.TempDir(), "set")
		if err := ExtractBackupSet(&archive, destination); err == nil {
			t.Fatalf("ExtractBackupSet accepted %q", name)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatalf("failed extraction left %s behind", destination)
		}
	}
}

func TestExtractBackupSetRejectsIncompleteArchives(t *testing.T) {
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	_ = writer.WriteHeader(&tar.Header{Name: SecretKeyFilename, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
	_, _ = writer.Write([]byte("x"))
	_ = writer.Close()
	_ = compressed.Close()
	destination := filepath.Join(t.TempDir(), "set")
	if err := ExtractBackupSet(bytes.NewReader(archive.Bytes()), destination); err == nil {
		t.Fatal("ExtractBackupSet accepted an archive without the database")
	}
	// An archive cut off before the gzip trailer fails too.
	if err := ExtractBackupSet(bytes.NewReader(archive.Bytes()[:archive.Len()-4]), filepath.Join(t.TempDir(), "set")); err == nil {
		t.Fatal("ExtractBackupSet accepted a truncated archive")
	}
}
