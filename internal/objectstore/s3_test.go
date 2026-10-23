package objectstore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/objectstore/s3test"
)

func TestPresignMatchesAWSExample(t *testing.T) {
	// The presigned URL example from the AWS Signature Version 4 docs.
	client := New(Destination{Bucket: "examplebucket", Region: "us-east-1",
		AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}, nil)
	client.now = func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }
	signed, err := client.PresignGet("test.txt", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(signed, "https://examplebucket.s3.amazonaws.com/test.txt?") ||
		!strings.Contains(signed, "&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404&") {
		t.Fatalf("presigned URL = %s", signed)
	}
}

func TestParseURL(t *testing.T) {
	for raw, want := range map[string][2]string{
		"s3://backups":               {"backups", ""},
		"s3://backups/":              {"backups", ""},
		"s3://my.backups/orch/prod/": {"my.backups", "orch/prod"},
		" s3://backups/production ":  {"backups", "production"},
	} {
		bucket, prefix, err := ParseURL(raw)
		if err != nil || bucket != want[0] || prefix != want[1] {
			t.Errorf("ParseURL(%q) = %q, %q, %v", raw, bucket, prefix, err)
		}
	}
	for _, raw := range []string{"backups", "https://backups", "s3://B", "s3://ab", "s3://backups/a/../b", "s3://backups/a//b", "s3://backups/a?b"} {
		if _, _, err := ParseURL(raw); err == nil {
			t.Errorf("ParseURL(%q) succeeded", raw)
		}
	}
}

func TestRegionFromEndpoint(t *testing.T) {
	for endpoint, want := range map[string]string{
		"https://abc.r2.cloudflarestorage.com":   "auto",
		"https://s3.us-west-004.backblazeb2.com": "us-west-004",
		"https://fsn1.your-objectstorage.com":    "fsn1",
		"https://s3.eu-central-1.amazonaws.com":  "eu-central-1",
		"https://fra1.digitaloceanspaces.com":    "fra1",
		"https://minio.example.com":              "",
	} {
		if got := RegionFromEndpoint(endpoint); got != want {
			t.Errorf("RegionFromEndpoint(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	for raw, want := range map[string]string{"": "", "minio.local:9000": "https://minio.local:9000", "http://127.0.0.1:9000/": "http://127.0.0.1:9000"} {
		if got, err := NormalizeEndpoint(raw); err != nil || got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"ftp://host", "https://user:pass@host", "https://host/path"} {
		if _, err := NormalizeEndpoint(raw); err == nil {
			t.Errorf("NormalizeEndpoint(%q) succeeded", raw)
		}
	}
}

func TestClientRoundTrip(t *testing.T) {
	server := s3test.New("backups")
	defer server.Close()
	ctx := context.Background()
	client := New(Destination{Bucket: "backups", Prefix: "orch", Endpoint: server.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	if region, err := client.DetectRegion(ctx); err != nil || region != "us-east-1" {
		t.Fatalf("DetectRegion = %q, %v", region, err)
	}
	key := client.Destination().Key("volumes/app/data/20261017T030000Z.tar.gz")
	if err := client.Put(ctx, key, strings.NewReader("archive"), 7); err != nil {
		t.Fatal(err)
	}
	objects, err := client.List(ctx, "orch/volumes/")
	if err != nil || len(objects) != 1 || objects[0].Key != key || objects[0].Size != 7 {
		t.Fatalf("List = %+v, %v", objects, err)
	}
	body, size, err := client.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(body)
	body.Close()
	if string(contents) != "archive" || size != 7 {
		t.Fatalf("Get = %q, %d", contents, size)
	}
	if err := client.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Get(ctx, key); err != ErrNotFound {
		t.Fatalf("Get after delete = %v", err)
	}
	if err := client.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing key = %v", err)
	}
}

func TestPresignedMultipartUpload(t *testing.T) {
	server := s3test.New("backups")
	defer server.Close()
	ctx := context.Background()
	client := New(Destination{Bucket: "backups", Endpoint: server.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	uploadID, err := client.CreateMultipartUpload(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	var etags []string
	for part, data := range []string{"hello ", "world"} {
		target, err := client.PresignUploadPart("big", uploadID, part+1, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPut, target, bytes.NewReader([]byte(data)))
		response, err := http.DefaultClient.Do(request)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("upload part: %v %v", response, err)
		}
		etags = append(etags, response.Header.Get("ETag"))
	}
	if err := client.CompleteMultipartUpload(ctx, "big", uploadID, etags); err != nil {
		t.Fatal(err)
	}
	if value, _ := server.Object("big"); string(value) != "hello world" {
		t.Fatalf("object = %q", value)
	}
	target, _ := client.PresignGet("big", time.Hour)
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(contents) != "hello world" {
		t.Fatalf("presigned get = %q", contents)
	}
}

func TestPresignedPathMatchesSignedPath(t *testing.T) {
	client := New(Destination{Bucket: "backups", Prefix: "team+prod", Endpoint: "https://minio.example.com", AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	signed, err := client.PresignGet(client.Destination().Key("a=b c.tar.gz"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(signed, "https://minio.example.com/backups/team%2Bprod/a%3Db%20c.tar.gz?") {
		t.Fatalf("presigned URL = %s", signed)
	}
}

func TestArchives(t *testing.T) {
	archives := Archives([]Object{{Key: "p/controller/20261017T030000Z.tar.gz"}, {Key: "p/controller/nested/x.tar.gz"},
		{Key: "p/controller/notes.txt"}, {Key: "p/other/20261017T030000Z.tar.gz"}}, "p/controller/")
	if len(archives) != 1 || archives["20261017T030000Z"].Key != "p/controller/20261017T030000Z.tar.gz" {
		t.Fatalf("Archives() = %v", archives)
	}
}

func TestUploadPart(t *testing.T) {
	server := s3test.New("backups")
	defer server.Close()
	ctx := context.Background()
	client := New(Destination{Bucket: "backups", Endpoint: server.URL, AccessKeyID: "id", SecretAccessKey: "secret"}, nil)
	uploadID, err := client.CreateMultipartUpload(ctx, "set")
	if err != nil {
		t.Fatal(err)
	}
	var etags []string
	for part, data := range []string{"controller ", "backup"} {
		etag, err := client.UploadPart(ctx, "set", uploadID, part+1, strings.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		etags = append(etags, etag)
	}
	if err := client.CompleteMultipartUpload(ctx, "set", uploadID, etags); err != nil {
		t.Fatal(err)
	}
	if value, _ := server.Object("set"); string(value) != "controller backup" {
		t.Fatalf("object = %q", value)
	}
}
