package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/TorstenDittmann/orch/internal/security"
)

const (
	// maxUploadParts bounds how many presigned part URLs one upload may carry.
	maxUploadParts = 10000
	partAttempts   = 3
)

// partRetryDelay grows with each retry of a part; tests shorten it.
var partRetryDelay = 2 * time.Second

// offsiteHTTP talks to the bucket through presigned URLs, so the agent never
// holds the bucket's credentials.
var offsiteHTTP = &http.Client{Transport: &http.Transport{
	Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 30 * time.Second,
	ResponseHeaderTimeout: 5 * time.Minute, IdleConnTimeout: time.Minute,
}}

func checkPresignedURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return errors.New("invalid presigned URL")
	}
	return nil
}

// upload copies a local backup to the bucket. One URL takes the whole
// archive; several take consecutive parts of partSize bytes and the ETag of
// each part is returned for completing the multipart upload.
// With encryptTo set, the archive is first encrypted to that public key in
// a scratch file, and the URLs cover the encrypted size.
func (v *volumeBackups) upload(ctx context.Context, application, volume, name string, urls []string, partSize uint64, encryptTo string) ([]string, error) {
	if len(urls) == 0 || len(urls) > maxUploadParts {
		return nil, errors.New("upload needs between 1 and 10000 URLs")
	}
	for _, target := range urls {
		if err := checkPresignedURL(target); err != nil {
			return nil, err
		}
	}
	path, err := v.path(application, volume, name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errVolumeBackupNotFound
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if encryptTo != "" {
		encrypted, cleanup, err := v.encryptForUpload(ctx, application, volume, file, encryptTo)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		file = encrypted
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := uint64(info.Size())
	if len(urls) == 1 {
		partSize = size
	} else if partSize == 0 || uint64(len(urls)-1)*partSize >= size || uint64(len(urls))*partSize < size {
		return nil, fmt.Errorf("%d parts of %d bytes do not cover a %d byte backup", len(urls), partSize, size)
	}
	etags := make([]string, 0, len(urls))
	for index, target := range urls {
		offset := uint64(index) * partSize
		length := min(partSize, size-offset)
		etag, err := putPartWithRetries(ctx, target, func() io.Reader { return io.NewSectionReader(file, int64(offset), int64(length)) }, int64(length))
		if err != nil {
			if len(urls) > 1 {
				return nil, fmt.Errorf("upload part %d of %d: %w", index+1, len(urls), err)
			}
			return nil, fmt.Errorf("upload: %w", err)
		}
		etags = append(etags, etag)
	}
	return etags, nil
}

// encryptForUpload writes archive encrypted to publicKey into a scratch
// file next to it and returns that file, rewound. Retried parts read the
// same bytes again, which a stream encrypted on the fly could not give.
func (v *volumeBackups) encryptForUpload(ctx context.Context, application, volume string, archive *os.File, publicKey string) (*os.File, func(), error) {
	if !security.ValidRecoveryPublicKey(publicKey) {
		return nil, nil, errors.New("upload names an invalid encryption key")
	}
	scratch, err := os.MkdirTemp(v.archiveDir(application, volume), ".partial-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(scratch) }
	encrypted, err := os.OpenFile(filepath.Join(scratch, "upload"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	closeAndClean := func() { _ = encrypted.Close(); cleanup() }
	if err := security.EncryptBackup(contextWriter{ctx, encrypted}, archive, publicKey, rand.Reader); err != nil {
		closeAndClean()
		return nil, nil, fmt.Errorf("encrypt backup: %w", err)
	}
	if _, err := encrypted.Seek(0, io.SeekStart); err != nil {
		closeAndClean()
		return nil, nil, err
	}
	return encrypted, closeAndClean, nil
}

// contextWriter stops a long write once ctx ends.
type contextWriter struct {
	ctx context.Context
	io.Writer
}

func (w contextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(data)
}

// putPartWithRetries retries a part a few times, so one dropped connection
// does not restart a large upload from its first part.
func putPartWithRetries(ctx context.Context, target string, body func() io.Reader, length int64) (string, error) {
	var err error
	for attempt := 0; attempt < partAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * partRetryDelay):
			}
		}
		var etag string
		if etag, err = putPart(ctx, target, body(), length); err == nil {
			return etag, nil
		}
	}
	return "", err
}

func putPart(ctx context.Context, target string, body io.Reader, length int64) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return "", err
	}
	request.ContentLength = length
	if length == 0 {
		request.Body = http.NoBody
	}
	response, err := offsiteHTTP.Do(request)
	if err != nil {
		return "", redactURLError(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return "", fmt.Errorf("bucket answered %s: %s", response.Status, boundedMessage(string(detail)))
	}
	return response.Header.Get("ETag"), nil
}

// stageFetch downloads a backup the node does not have yet without holding
// the volume's lock, so a long download does not delay scheduled backups.
// The caller takes the lock and calls publish to move it into place, which
// keeps a copy that appeared meanwhile. It always downloads: a local copy
// seen before the lock is taken could be pruned before it is used. An
// encrypted copy is opened with contentKey and kept in plaintext.
func (v *volumeBackups) stageFetch(ctx context.Context, application, volume, name, source string, contentKey []byte) (VolumeBackup, func() error, func(), error) {
	backup, ok := parseVolumeBackupName(name + volumeBackupExtension)
	if !ok {
		return VolumeBackup{}, nil, nil, errVolumeBackupNotFound
	}
	if err := checkPresignedURL(source); err != nil {
		return VolumeBackup{}, nil, nil, err
	}
	directory := v.archiveDir(application, volume)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return VolumeBackup{}, nil, nil, err
	}
	scratch, err := os.MkdirTemp(directory, ".partial-")
	if err != nil {
		return VolumeBackup{}, nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(scratch) }
	staged := filepath.Join(scratch, "archive"+volumeBackupExtension)
	size, err := download(ctx, source, staged)
	if err != nil {
		cleanup()
		return VolumeBackup{}, nil, nil, fmt.Errorf("download backup %s from the bucket: %w", name, err)
	}
	if size, err = decryptStaged(staged, contentKey); err != nil {
		cleanup()
		return VolumeBackup{}, nil, nil, fmt.Errorf("open backup %s from the bucket: %w", name, err)
	}
	backup.Size = size
	publish := func() error {
		final := filepath.Join(directory, name+volumeBackupExtension)
		if _, err := os.Lstat(final); err == nil {
			return nil
		}
		return os.Rename(staged, final)
	}
	return backup, publish, cleanup, nil
}

// decryptStaged replaces an encrypted download with its plaintext and
// returns the size of what is left. Copies made before backups were
// encrypted are left as they are.
func decryptStaged(path string, contentKey []byte) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	if !security.IsEncryptedBackup(reader) {
		info, err := file.Stat()
		if err != nil {
			return 0, err
		}
		return info.Size(), nil
	}
	if len(contentKey) == 0 {
		return 0, errors.New("the copy is encrypted and the controller sent no key for it")
	}
	plaintext, err := security.DecryptBackupWithContentKey(reader, contentKey)
	if err != nil {
		return 0, err
	}
	decrypted := path + ".plain"
	output, err := os.OpenFile(decrypted, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	size, copyErr := io.Copy(output, plaintext)
	if syncErr := output.Sync(); copyErr == nil {
		copyErr = syncErr
	}
	if closeErr := output.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil {
		copyErr = os.Rename(decrypted, path)
	}
	if copyErr != nil {
		_ = os.Remove(decrypted)
		return 0, copyErr
	}
	return size, nil
}

// restoreFromBucket restores a backup this node does not have: it is
// downloaded first, then moved into place and restored under one hold of
// the volume's lock, so pruning cannot remove it in between.
func (v *volumeBackups) restoreFromBucket(ctx context.Context, application, volume, name, source, allocationID string, contentKey []byte) (VolumeBackup, error) {
	_, publish, cleanup, err := v.stageFetch(ctx, application, volume, name, source, contentKey)
	if err != nil {
		return VolumeBackup{}, err
	}
	defer cleanup()
	unlock := v.lock(application, volume)
	defer unlock()
	if err := publish(); err != nil {
		return VolumeBackup{}, err
	}
	return v.restoreLocked(ctx, application, volume, name, allocationID)
}

func download(ctx context.Context, source, destination string) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return 0, err
	}
	// Asking for the stored bytes keeps the transport from decompressing
	// the archive.
	request.Header.Set("Accept-Encoding", "identity")
	response, err := offsiteHTTP.Do(request)
	if err != nil {
		return 0, redactURLError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("bucket answered %s", response.Status)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	size, copyErr := io.Copy(file, response.Body)
	if syncErr := file.Sync(); copyErr == nil {
		copyErr = syncErr
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil && response.ContentLength >= 0 && size != response.ContentLength {
		copyErr = fmt.Errorf("download ended after %d of %d bytes", size, response.ContentLength)
	}
	return size, copyErr
}

// redactURLError keeps presigned signatures out of error messages.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
