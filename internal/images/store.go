package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

// MaxUploadBytes bounds one image archive that orch deploy uploads.
const MaxUploadBytes int64 = 32 << 30

// MinFreeBytes is the free space an upload must leave on the controller's
// disk, so uploads cannot starve the controller database next to them.
const MinFreeBytes uint64 = 2 << 30

// uploadGrace keeps unreferenced uploads and partial blobs for a while, so a
// deploy that just uploaded or found an image can still use it.
const uploadGrace = time.Hour

const indexFile = "uploads.json"

var localIDPattern = regexp.MustCompile(`^(sha256:)?[a-f0-9]{64}$`)

// ErrArchive reports an upload that is not an image archive orch can use.
var ErrArchive = errors.New("upload is not a docker image save archive with the named image")

// ErrNoSpace reports an upload that would leave too little free disk space.
var ErrNoSpace = errors.New("the controller's disk is too full to store the image")

// Store keeps the images orch deploy uploads from a local container engine,
// so the controller can serve them to agents without a registry. Blobs live in
// a containerd content store; uploads.json maps each local image ID to the
// digest it was stored as, so an unchanged image is not uploaded again.
type Store struct {
	root    string
	content content.Store
	now     func() time.Time
	// imports serializes imports with garbage collection.
	imports sync.Mutex
	// index guards uploads.json.
	index sync.Mutex
	// freeBytes reports the free space below root.
	freeBytes func(string) (uint64, error)
}

// upload records one stored local image. Owner is the application a
// deployer credential is limited to, or empty for administrators, so a
// deployer for one application cannot hand its image to another.
type upload struct {
	Owner   string    `json:"owner,omitempty"`
	LocalID string    `json:"local_id"`
	Digest  string    `json:"digest"`
	UsedAt  time.Time `json:"used_at"`
}

// OpenStore opens or creates the image store in root.
func OpenStore(root string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create image store: %w", err)
	}
	// containerd's libraries log through logrus's standard logger, which orch
	// does not otherwise use. Keep their warnings, such as the content store's
	// fsverity probe on file systems without it, out of controller logs.
	logrus.StandardLogger().SetLevel(logrus.ErrorLevel)
	store, err := local.NewStore(root)
	if err != nil {
		return nil, fmt.Errorf("open image store: %w", err)
	}
	return &Store{root: root, content: store, now: now, freeBytes: freeBytes}, nil
}

// ValidLocalID reports whether id looks like an image ID a container engine
// reports.
func ValidLocalID(id string) bool { return localIDPattern.MatchString(id) }

// Lookup returns the digest owner's upload of a local image was stored as, or
// "" when there is none. A hit counts as a use, so collection keeps the image.
func (s *Store) Lookup(ctx context.Context, owner, localID string) (string, error) {
	s.index.Lock()
	defer s.index.Unlock()
	uploads, err := s.readIndex()
	if err != nil {
		return "", err
	}
	for i, item := range uploads {
		if item.Owner != owner || item.LocalID != localID {
			continue
		}
		if !s.Has(ctx, item.Digest) {
			return "", nil
		}
		uploads[i].UsedAt = s.now()
		return item.Digest, s.writeIndex(uploads)
	}
	return "", nil
}

// Has reports whether the store holds a manifest or index with this digest.
func (s *Store) Has(ctx context.Context, value string) bool {
	parsed, err := digest.Parse(value)
	if err != nil {
		return false
	}
	_, err = s.content.Info(ctx, parsed)
	return err == nil
}

// Import stores the image named image from a docker image save archive for
// owner and returns the digest to pin it to and the platforms it runs on.
func (s *Store) Import(ctx context.Context, owner, image, localID string, archiveReader io.Reader) (string, []string, error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", nil, fmt.Errorf("%w: invalid image reference", ErrArchive)
	}
	if !ValidLocalID(localID) {
		return "", nil, fmt.Errorf("%w: invalid local image ID", ErrArchive)
	}
	s.imports.Lock()
	defer s.imports.Unlock()
	guarded := &spaceReader{reader: io.LimitReader(archiveReader, MaxUploadBytes), check: s.checkSpace}
	if err := s.checkSpace(); err != nil {
		return "", nil, err
	}
	index, err := archive.ImportIndex(ctx, s.content, guarded, archive.WithImportCompression())
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		if guarded.err != nil {
			return "", nil, guarded.err
		}
		return "", nil, fmt.Errorf("%w: %v", ErrArchive, err)
	}
	target, err := s.selectImage(ctx, index, reference.TagNameOnly(named).String())
	if err != nil {
		return "", nil, err
	}
	platforms, err := s.platforms(ctx, target, 0)
	if err != nil {
		return "", nil, err
	}
	if len(platforms) == 0 {
		return "", nil, ErrPlatform
	}
	s.index.Lock()
	defer s.index.Unlock()
	uploads, err := s.readIndex()
	if err != nil {
		return "", nil, err
	}
	kept := uploads[:0]
	for _, item := range uploads {
		if item.Owner != owner || item.LocalID != localID {
			kept = append(kept, item)
		}
	}
	kept = append(kept, upload{Owner: owner, LocalID: localID, Digest: target.Digest.String(), UsedAt: s.now()})
	if err := s.writeIndex(kept); err != nil {
		return "", nil, err
	}
	return target.Digest.String(), platforms, nil
}

func (s *Store) checkSpace() error {
	free, err := s.freeBytes(s.root)
	if err != nil {
		return fmt.Errorf("check free space for the image store: %w", err)
	}
	if free < MinFreeBytes {
		return ErrNoSpace
	}
	return nil
}

// spaceReader stops an upload that would fill the disk. Stored data can be
// twice the archive size while uncompressed layers wait for collection.
type spaceReader struct {
	reader io.Reader
	check  func() error
	read   int64
	err    error
}

func (r *spaceReader) Read(buffer []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.reader.Read(buffer)
	before := r.read
	r.read += int64(n)
	if before/(256<<20) != r.read/(256<<20) {
		if r.err = r.check(); r.err != nil {
			return n, r.err
		}
	}
	return n, err
}

// selectImage finds the named image in an imported archive index. When the
// image is itself an index whose platforms were not all saved, which is what
// docker image save writes for a multi-platform image it pulled one platform
// of, it picks the first saved platform orch supports instead.
func (s *Store) selectImage(ctx context.Context, index ocispec.Descriptor, name string) (ocispec.Descriptor, error) {
	var parsed ocispec.Index
	if err := s.readJSON(ctx, index, &parsed); err != nil {
		return ocispec.Descriptor{}, err
	}
	var target *ocispec.Descriptor
	for i, child := range parsed.Manifests {
		if child.Annotations[images.AnnotationImageName] == name {
			target = &parsed.Manifests[i]
			break
		}
	}
	if target == nil && len(parsed.Manifests) == 1 {
		target = &parsed.Manifests[0]
	}
	if target == nil {
		return ocispec.Descriptor{}, fmt.Errorf("%w: the archive does not contain %s", ErrArchive, name)
	}
	if !images.IsIndexType(target.MediaType) {
		return *target, nil
	}
	var nested ocispec.Index
	if err := s.readJSON(ctx, *target, &nested); err != nil {
		return ocispec.Descriptor{}, err
	}
	var present []ocispec.Descriptor
	for _, child := range nested.Manifests {
		if _, err := s.content.Info(ctx, child.Digest); err == nil {
			present = append(present, child)
		}
	}
	if len(present) == len(nested.Manifests) {
		return *target, nil
	}
	for _, child := range present {
		if child.Platform != nil && platformSupported(*child.Platform) {
			return child, nil
		}
	}
	return ocispec.Descriptor{}, ErrPlatform
}

// platforms lists the supported platforms of a stored manifest or index.
func (s *Store) platforms(ctx context.Context, desc ocispec.Descriptor, depth int) ([]string, error) {
	if depth > 3 {
		return nil, ErrInvalid
	}
	if images.IsIndexType(desc.MediaType) {
		var index ocispec.Index
		if err := s.readJSON(ctx, desc, &index); err != nil {
			return nil, err
		}
		var result []string
		for _, child := range index.Manifests {
			if child.Platform != nil {
				if platformSupported(*child.Platform) {
					result = append(result, platformString(*child.Platform))
				}
				continue
			}
			nested, err := s.platforms(ctx, child, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, nested...)
		}
		sort.Strings(result)
		return result, nil
	}
	if !images.IsManifestType(desc.MediaType) {
		return nil, fmt.Errorf("%w: unsupported media type %s", ErrArchive, desc.MediaType)
	}
	var manifest ocispec.Manifest
	if err := s.readJSON(ctx, desc, &manifest); err != nil {
		return nil, err
	}
	var config ocispec.Image
	if err := s.readJSON(ctx, manifest.Config, &config); err != nil {
		return nil, err
	}
	if !platformSupported(config.Platform) {
		return nil, nil
	}
	return []string{platformString(config.Platform)}, nil
}

func platformString(p ocispec.Platform) string {
	if p.Variant != "" {
		return p.OS + "/" + p.Architecture + "/" + p.Variant
	}
	return p.OS + "/" + p.Architecture
}

func (s *Store) readJSON(ctx context.Context, desc ocispec.Descriptor, target any) error {
	data, err := s.readSmall(ctx, desc.Digest)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, target) != nil {
		return fmt.Errorf("%w: invalid image metadata", ErrArchive)
	}
	return nil
}

// readSmall reads a manifest, index or config blob.
func (s *Store) readSmall(ctx context.Context, value digest.Digest) ([]byte, error) {
	const limit = 4 << 20
	info, err := s.content.Info(ctx, value)
	if err != nil {
		return nil, fmt.Errorf("%w: missing blob %s", ErrArchive, value)
	}
	if info.Size > limit {
		return nil, ErrInvalid
	}
	return content.ReadBlob(ctx, s.content, ocispec.Descriptor{Digest: value, Size: info.Size})
}

// Collect removes uploads that no deployment in keep references and that were
// not used within the grace period, then deletes every blob no remaining
// upload needs.
func (s *Store) Collect(ctx context.Context, keep map[string]bool) error {
	// An upload in progress holds the lock; collect on a later round.
	if !s.imports.TryLock() {
		return nil
	}
	defer s.imports.Unlock()
	cutoff := s.now().Add(-uploadGrace)
	s.index.Lock()
	uploads, err := s.readIndex()
	if err != nil {
		s.index.Unlock()
		return err
	}
	kept := uploads[:0]
	for _, item := range uploads {
		if keep[item.Digest] || item.UsedAt.After(cutoff) {
			kept = append(kept, item)
		}
	}
	err = s.writeIndex(kept)
	s.index.Unlock()
	if err != nil {
		return err
	}
	reachable := map[digest.Digest]bool{}
	var mark func(digest.Digest, int)
	mark = func(value digest.Digest, depth int) {
		if reachable[value] || depth > 4 {
			return
		}
		reachable[value] = true
		data, err := s.readSmall(ctx, value)
		if err != nil {
			return
		}
		var node struct {
			Config    *ocispec.Descriptor  `json:"config"`
			Layers    []ocispec.Descriptor `json:"layers"`
			Manifests []ocispec.Descriptor `json:"manifests"`
		}
		if json.Unmarshal(data, &node) != nil {
			return
		}
		if node.Config != nil {
			reachable[node.Config.Digest] = true
		}
		for _, layer := range node.Layers {
			reachable[layer.Digest] = true
		}
		for _, child := range node.Manifests {
			mark(child.Digest, depth+1)
		}
	}
	// Deployed digests keep their blobs even when no upload record names
	// them any more, for example after the same local image was re-uploaded.
	for value := range keep {
		if parsed, err := digest.Parse(value); err == nil {
			if _, err := s.content.Info(ctx, parsed); err == nil {
				mark(parsed, 0)
			}
		}
	}
	for _, item := range kept {
		if parsed, err := digest.Parse(item.Digest); err == nil {
			mark(parsed, 0)
		}
	}
	var garbage []digest.Digest
	err = s.content.Walk(ctx, func(info content.Info) error {
		if !reachable[info.Digest] && info.CreatedAt.Before(cutoff) {
			garbage = append(garbage, info.Digest)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk image store: %w", err)
	}
	for _, value := range garbage {
		if err := s.content.Delete(ctx, value); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("delete image blob: %w", err)
		}
	}
	statuses, err := s.content.ListStatuses(ctx)
	if err != nil {
		return fmt.Errorf("list partial image uploads: %w", err)
	}
	for _, status := range statuses {
		if status.UpdatedAt.Before(cutoff) {
			_ = s.content.Abort(ctx, status.Ref)
		}
	}
	return nil
}

func (s *Store) readIndex() ([]upload, error) {
	data, err := os.ReadFile(filepath.Join(s.root, indexFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read image store index: %w", err)
	}
	var uploads []upload
	if err := json.Unmarshal(data, &uploads); err != nil {
		// The index only saves re-uploads; losing it must not block deploys.
		log.Printf("controller: image store index is damaged and was reset: %v", err)
		return nil, nil
	}
	return uploads, nil
}

func (s *Store) writeIndex(uploads []upload) error {
	data, err := json.Marshal(uploads)
	if err != nil {
		return err
	}
	temporary := filepath.Join(s.root, indexFile+".tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write image store index: %w", err)
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write image store index: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(s.root, indexFile)); err != nil {
		return fmt.Errorf("write image store index: %w", err)
	}
	return nil
}
