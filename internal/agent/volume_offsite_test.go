package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/objectstore"
	"github.com/TorstenDittmann/orch/internal/objectstore/s3test"
	"github.com/TorstenDittmann/orch/internal/security"
)

func TestVolumeBackupUploadsInPartsAndFetchesBack(t *testing.T) {
	ctx := context.Background()
	bucket := s3test.New("backups")
	defer bucket.Close()
	client := objectstore.New(objectstore.Destination{Bucket: "backups", Endpoint: bucket.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	now := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	backups := newVolumeBackups(t.TempDir(), nil, nil, func() time.Time { return now })
	source := backups.volumeDir("game", "data")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	world := make([]byte, 256<<10)
	_, _ = rand.Read(world)
	if err := os.WriteFile(filepath.Join(source, "world.dat"), world, 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := backups.archive(ctx, "game", "data", source, "")
	if err != nil {
		t.Fatal(err)
	}
	local, _ := os.ReadFile(filepath.Join(backups.archiveDir("game", "data"), backup.Name+volumeBackupExtension))

	uploadID, err := client.CreateMultipartUpload(ctx, "copy")
	if err != nil {
		t.Fatal(err)
	}
	partSize := uint64(backup.Size/3 + 1)
	var urls []string
	for part := 1; part <= 3; part++ {
		url, _ := client.PresignUploadPart("copy", uploadID, part, time.Hour)
		urls = append(urls, url)
	}
	partRetryDelay = time.Millisecond
	bucket.FailNext(2)
	etags, err := backups.upload(ctx, "game", "data", backup.Name, urls, partSize, "")
	if err != nil || len(etags) != 3 {
		t.Fatalf("upload() = %v, %v", etags, err)
	}
	if err := client.CompleteMultipartUpload(ctx, "copy", uploadID, etags); err != nil {
		t.Fatal(err)
	}
	if copied, _ := bucket.Object("copy"); !bytes.Equal(copied, local) {
		t.Fatal("the bucket's copy differs from the archive")
	}
	if _, err := backups.upload(ctx, "game", "data", backup.Name, urls[:2], partSize, ""); err == nil {
		t.Fatal("upload accepted parts that do not cover the archive")
	}

	url, _ := client.PresignGet("copy", time.Hour)
	if _, _, _, err := backups.stageFetch(ctx, "game", "data", "../escape", url, nil); err == nil {
		t.Fatal("stageFetch accepted an invalid backup name")
	}
}

func TestVolumeBackupUploadsEncryptedAndRestoresPlaintext(t *testing.T) {
	ctx := context.Background()
	bucket := s3test.New("backups")
	defer bucket.Close()
	client := objectstore.New(objectstore.Destination{Bucket: "backups", Endpoint: bucket.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	now := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	backups := newVolumeBackups(t.TempDir(), nil, nil, func() time.Time { return now })
	source := backups.volumeDir("game", "data")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	world := bytes.Repeat([]byte("secret world "), 20000)
	if err := os.WriteFile(filepath.Join(source, "world.dat"), world, 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := backups.archive(ctx, "game", "data", source, "")
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(backups.archiveDir("game", "data"), backup.Name+volumeBackupExtension)
	local, _ := os.ReadFile(archive)
	private, public, err := security.NewRecoveryKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	url, _ := client.PresignPut("copy", time.Hour)
	if _, err := backups.upload(ctx, "game", "data", backup.Name, []string{url}, 0, public); err != nil {
		t.Fatal(err)
	}
	copied, _ := bucket.Object("copy")
	if uint64(len(copied)) != security.EncryptedBackupSize(uint64(len(local))) || bytes.Contains(copied, local[:64]) {
		t.Fatalf("the bucket's copy is %d bytes and not encrypted as expected", len(copied))
	}
	if entries, _ := os.ReadDir(backups.archiveDir("game", "data")); len(entries) != 1 {
		t.Fatalf("upload left %d entries behind, want only the archive", len(entries))
	}

	key, err := security.BackupContentKey(copied[:security.EncryptedBackupHeaderSize], private)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	get, _ := client.PresignGet("copy", time.Hour)
	if _, _, _, err := backups.stageFetch(ctx, "game", "data", backup.Name, get, nil); err == nil {
		t.Fatal("stageFetch opened an encrypted copy without a key")
	}
	fetched, publish, cleanup, err := backups.stageFetch(ctx, "game", "data", backup.Name, get, key)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := publish(); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(archive)
	if !bytes.Equal(restored, local) || fetched.Size != int64(len(local)) {
		t.Fatalf("fetched copy differs from the archive: %d bytes, size %d", len(restored), fetched.Size)
	}
}
