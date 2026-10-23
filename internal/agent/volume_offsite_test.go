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
	etags, err := backups.upload(ctx, "game", "data", backup.Name, urls, partSize)
	if err != nil || len(etags) != 3 {
		t.Fatalf("upload() = %v, %v", etags, err)
	}
	if err := client.CompleteMultipartUpload(ctx, "copy", uploadID, etags); err != nil {
		t.Fatal(err)
	}
	if copied, _ := bucket.Object("copy"); !bytes.Equal(copied, local) {
		t.Fatal("the bucket's copy differs from the archive")
	}
	if _, err := backups.upload(ctx, "game", "data", backup.Name, urls[:2], partSize); err == nil {
		t.Fatal("upload accepted parts that do not cover the archive")
	}

	url, _ := client.PresignGet("copy", time.Hour)
	if _, _, _, err := backups.stageFetch(ctx, "game", "data", "../escape", url); err == nil {
		t.Fatal("stageFetch accepted an invalid backup name")
	}
}
