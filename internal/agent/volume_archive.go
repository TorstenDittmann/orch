package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	modernsqlite "modernc.org/sqlite"
)

// sqliteHeader starts every SQLite database file, libSQL's included.
var sqliteHeader = []byte("SQLite format 3\x00")

type sqliteBackuper interface {
	NewBackup(string) (*modernsqlite.Backup, error)
}

// archiveVolume writes source as a gzip-compressed tar to destination. SQLite
// databases are copied with SQLite's online backup so a running writer cannot
// leave a torn copy; their -wal, -shm and -journal files are then left out
// because the copy already contains them. Other files are copied as they are.
func archiveVolume(ctx context.Context, source, destination, scratch string) (finalErr error) {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); finalErr == nil {
			finalErr = closeErr
		}
		if finalErr != nil {
			_ = os.Remove(destination)
		}
	}()
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	snapshotted := make(map[string]bool)
	err = filepath.WalkDir(source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if snapshotted[strings.TrimSuffix(relative, suffix)] && strings.HasSuffix(relative, suffix) {
				return nil
			}
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		link := ""
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(name); err != nil {
				return err
			}
		case info.IsDir(), info.Mode().IsRegular():
		default:
			// Sockets, devices and pipes cannot be restored meaningfully.
			return nil
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = relative
		if info.IsDir() {
			header.Name += "/"
		}
		header.Uname, header.Gname = "", ""
		header.Format = tar.FormatPAX
		if !info.Mode().IsRegular() {
			return archive.WriteHeader(header)
		}
		contents := name
		if isSQLite(name) {
			snapshot, err := snapshotSQLite(ctx, name, scratch)
			if err == nil {
				defer os.Remove(snapshot)
				if snapshotInfo, statErr := os.Stat(snapshot); statErr == nil {
					contents = snapshot
					header.Size = snapshotInfo.Size()
					snapshotted[relative] = true
				}
			}
		}
		return writeArchiveFile(archive, header, contents)
	})
	if err != nil {
		return err
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	return file.Sync()
}

// writeArchiveFile writes exactly header.Size bytes. A file that shrank while
// it was read is padded so the archive stays readable.
func writeArchiveFile(archive *tar.Writer, header *tar.Header, name string) error {
	file, err := openRegular(name)
	if errors.Is(err, errNotRegular) || errors.Is(err, fs.ErrNotExist) {
		// The workload replaced or removed the file after the walk saw it.
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	written, err := io.Copy(archive, io.LimitReader(file, header.Size))
	if err != nil {
		return err
	}
	if written < header.Size {
		_, err = io.CopyN(archive, zeroReader{}, header.Size-written)
	}
	return err
}

var errNotRegular = errors.New("not a regular file")

// openRegular opens a file the walk saw as regular. The volume belongs to a
// running workload, so the name may since have become a FIFO, a device or a
// symlink: O_NONBLOCK keeps a FIFO from blocking the open, O_NOFOLLOW refuses
// a final symlink, and the descriptor itself must still be a regular file.
func openRegular(name string) (*os.File, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|archiveOpenFlags, 0)
	if isSymlinkLoop(err) {
		return nil, errNotRegular
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errNotRegular
	}
	return file, nil
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func isSQLite(name string) bool {
	file, err := openRegular(name)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return bytes.Equal(header, sqliteHeader)
}

// snapshotSQLite copies a live database with SQLite's online backup. It opens
// the database read-only so the copy never changes its journal mode.
func snapshotSQLite(ctx context.Context, name, scratch string) (string, error) {
	temporary, err := os.CreateTemp(scratch, "sqlite-*.db")
	if err != nil {
		return "", err
	}
	snapshot := temporary.Name()
	_ = temporary.Close()
	fail := func(err error) (string, error) {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(snapshot + suffix)
		}
		return "", err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(name), RawQuery: "mode=ro&_pragma=busy_timeout(10000)"}).String())
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	connection, err := db.Conn(ctx)
	if err != nil {
		return fail(err)
	}
	defer connection.Close()
	err = connection.Raw(func(driverConnection any) error {
		backuper, ok := driverConnection.(sqliteBackuper)
		if !ok {
			return errors.New("SQLite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(snapshot)
		if err != nil {
			return err
		}
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				_ = backup.Finish()
				return err
			}
			if more, err = backup.Step(256); err != nil {
				_ = backup.Finish()
				return err
			}
		}
		return backup.Finish()
	})
	if err != nil {
		return fail(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Remove(snapshot + suffix)
	}
	return snapshot, nil
}

// extractVolume unpacks an archive written by archiveVolume into destination,
// which must be an empty directory. It keeps ownership, modes and times and
// refuses entries that would land outside destination.
func extractVolume(ctx context.Context, archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read backup archive: %w", err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	type directoryTimes struct {
		path    string
		modTime time.Time
	}
	var directories []directoryTimes
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read backup archive: %w", err)
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("backup archive entry %q is outside the volume", header.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if err := requireNoSymlinkParents(destination, target); err != nil {
			return err
		}
		mode := fs.FileMode(header.Mode).Perm() | fs.FileMode(header.Mode)&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)
		switch header.Typeflag {
		case tar.TypeDir:
			if name == "." {
				if err := os.Chmod(target, 0o700); err != nil {
					return err
				}
			} else if err := os.Mkdir(target, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			directories = append(directories, directoryTimes{target, header.ModTime})
		case tar.TypeReg:
			output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(output, archive); err != nil {
				_ = output.Close()
				return err
			}
			if err := output.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		default:
			continue
		}
		if err := os.Lchown(target, header.Uid, header.Gid); err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		if header.Typeflag == tar.TypeSymlink {
			continue
		}
		// The volume root stays private; the runtime requires it.
		if name == "." {
			mode = 0o700
		}
		if err := os.Chmod(target, mode); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg {
			if err := os.Chtimes(target, header.ModTime, header.ModTime); err != nil {
				return err
			}
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		_ = os.Chtimes(directories[index].path, directories[index].modTime, directories[index].modTime)
	}
	return nil
}

func requireNoSymlinkParents(root, target string) error {
	relative, err := filepath.Rel(root, filepath.Dir(target))
	if err != nil || relative == "." {
		return err
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("backup archive entry %q has no parent directory", target)
		}
		if !info.IsDir() {
			return fmt.Errorf("backup archive entry %q is below a non-directory", target)
		}
	}
	return nil
}
