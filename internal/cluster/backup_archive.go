package cluster

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// maxBackupArchiveFile bounds one file unpacked from a backup set archive.
const maxBackupArchiveFile = 16 << 30

// backupSetFiles are the only files a backup set archive may hold.
func backupSetFiles() []string {
	files := []string{DatabaseFilename, SecretKeyFilename}
	for _, name := range controllerPKIFiles {
		files = append(files, path.Join(controllerIdentityDirectory, name))
	}
	return files
}

// ArchiveBackupSet writes the backup set in directory to w as a
// gzip-compressed tar, for copying it off the controller. Only the secret
// key may be missing, as it is in sets of clusters without one; any other
// missing file fails, so a set pruned while it is read is never copied
// incomplete.
func ArchiveBackupSet(directory string, w io.Writer) error {
	compressed := gzip.NewWriter(w)
	archive := tar.NewWriter(compressed)
	for _, name := range backupSetFiles() {
		file, err := os.Open(filepath.Join(directory, filepath.FromSlash(name)))
		if errors.Is(err, os.ErrNotExist) && name == SecretKeyFilename {
			continue
		}
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("%s is not a regular file", name)
		}
		if err == nil {
			err = archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = io.Copy(archive, file)
		}
		file.Close()
		if err != nil {
			return fmt.Errorf("archive %s: %w", name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return compressed.Close()
}

// ExtractBackupSet unpacks an archive written by ArchiveBackupSet into the
// new directory, which must not exist. Anything but the files of a backup
// set is rejected.
func ExtractBackupSet(r io.Reader, directory string) (finalErr error) {
	allowed := make(map[string]bool)
	for _, name := range backupSetFiles() {
		allowed[name] = true
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return err
	}
	defer func() {
		if finalErr != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Mkdir(filepath.Join(directory, controllerIdentityDirectory), 0o700); err != nil {
		return err
	}
	compressed, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read backup archive: %w", err)
	}
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read backup archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || !allowed[header.Name] || header.Size < 0 || header.Size > maxBackupArchiveFile {
			return fmt.Errorf("backup archive holds unexpected entry %q", header.Name)
		}
		delete(allowed, header.Name)
		file, err := os.OpenFile(filepath.Join(directory, filepath.FromSlash(header.Name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.CopyN(file, archive, header.Size)
		if syncErr := file.Sync(); err == nil {
			err = syncErr
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("unpack %s: %w", header.Name, err)
		}
	}
	// Reading to the end checks the gzip trailer, so a cut-off archive fails.
	// Only the one gzip member is read, so appended data costs nothing.
	compressed.Multistream(false)
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return fmt.Errorf("read backup archive: %w", err)
	}
	delete(allowed, SecretKeyFilename)
	for name := range allowed {
		return fmt.Errorf("backup archive is incomplete: it has no %s", name)
	}
	return nil
}
