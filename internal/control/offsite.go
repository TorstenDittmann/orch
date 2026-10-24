package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/objectstore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	offsiteSyncEvery      = 15 * time.Minute
	offsitePresignExpiry  = 12 * time.Hour
	offsiteTransferLimit  = 6 * time.Hour
	offsiteRequestTimeout = time.Minute
	offsiteSinglePutLimit = 1 << 30
	offsiteMinPartSize    = 128 << 20
	offsiteMaxParts       = 1000
	// offsiteStartupGrace is how long after the controller starts a node
	// that has not reconnected yet is not reported as holding up copies.
	offsiteStartupGrace = 5 * time.Minute
)

// isScheduledBackup reports whether a backup was taken on schedule.
// Labeled backups (manual, before-restore) have a suffix and are never
// pruned.
func isScheduledBackup(name string) bool { return !strings.Contains(name, "-") }

// offsiteBackups copies every backup to the cluster's S3 bucket, when one is
// set. The controller keeps the bucket's credentials: it uploads its own
// backup sets itself and hands workers presigned URLs for their volume
// backups. A sync runs every 15 minutes, after each controller backup and
// right after a manual backup, and uploads whatever the bucket is missing,
// so a failed upload is retried on the next run.
type offsiteBackups struct {
	service *service
	// dataDir holds the controller's backup sets; keep is how many of them
	// the bucket keeps. Both are zero when controller backups are off.
	dataDir   string
	keep      int
	wake      chan struct{}
	startedAt time.Time

	mu sync.Mutex
	// cancelSync stops the running sync, when the destination changes.
	cancelSync context.CancelFunc
	// url is the destination of the last sync, for status, and identity
	// also tells apart endpoints and keys for the same URL.
	url      string
	identity string
	syncedAt time.Time
	failedAt time.Time
	failure  string
	// failing holds the volumes whose last upload failed, so a failure that
	// repeats raises one event.
	failing map[string]bool
}

type offsiteStatus struct {
	URL      string
	SyncedAt time.Time
	FailedAt time.Time
	Failure  string
}

func newOffsiteBackups(service *service, dataDir string, keep int) *offsiteBackups {
	if keep <= 0 {
		dataDir, keep = "", 0
	}
	return &offsiteBackups{service: service, dataDir: dataDir, keep: keep, wake: make(chan struct{}, 1),
		startedAt: service.now(), failing: make(map[string]bool)}
}

func (o *offsiteBackups) status() offsiteStatus {
	if o == nil {
		return offsiteStatus{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return offsiteStatus{URL: o.url, SyncedAt: o.syncedAt, FailedAt: o.failedAt, Failure: o.failure}
}

// interrupt stops a running sync, which may still be copying to the old
// destination, and starts a new one.
func (o *offsiteBackups) interrupt() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.cancelSync != nil {
		o.cancelSync()
	}
	o.mu.Unlock()
	o.kick()
}

// kick asks for a sync now.
func (o *offsiteBackups) kick() {
	if o == nil {
		return
	}
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *offsiteBackups) run(ctx context.Context) {
	ticker := time.NewTicker(offsiteSyncEvery)
	defer ticker.Stop()
	for {
		syncContext, cancel := context.WithCancel(ctx)
		o.mu.Lock()
		o.cancelSync = cancel
		o.mu.Unlock()
		o.syncOnce(syncContext)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-o.wake:
		}
	}
}

// offsiteClient returns a client for the stored destination, or nil when
// none is set.
func (s *service) offsiteClient(ctx context.Context) (*objectstore.Client, error) {
	destination, found, err := s.state.BackupDestination(ctx, s.secretBox)
	if err != nil || !found {
		return nil, err
	}
	return objectstore.New(destination, nil), nil
}

// withTimeout bounds one bucket request, so a stalled connection cannot
// hold up the sync or shutdown.
func withTimeout(ctx context.Context, limit time.Duration, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return call(ctx)
}

// timed is withTimeout for a call that returns a value.
func timed[T any](ctx context.Context, limit time.Duration, call func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return call(ctx)
}

func listObjects(ctx context.Context, client *objectstore.Client, prefix string, limit time.Duration) ([]objectstore.Object, error) {
	return timed(ctx, limit, func(ctx context.Context) ([]objectstore.Object, error) { return client.List(ctx, prefix) })
}

// listArchives returns the names of the archives directly below prefix.
func listArchives(ctx context.Context, client *objectstore.Client, prefix string) (map[string]bool, error) {
	objects, err := listObjects(ctx, client, prefix, offsiteRequestTimeout)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for name := range objectstore.Archives(objects, prefix) {
		names[name] = true
	}
	return names, nil
}

func (o *offsiteBackups) syncOnce(ctx context.Context) {
	client, err := o.service.offsiteClient(ctx)
	if client == nil && err == nil {
		o.reset("", "")
		return
	}
	if client != nil {
		destination := client.Destination()
		o.reset(destination.URL(), destination.URL()+"\x00"+destination.Endpoint+"\x00"+destination.AccessKeyID)
	}
	var failures []string
	incomplete := false
	if err != nil && ctx.Err() != nil {
		return
	}
	if err != nil {
		// Keep the status visible even though the destination is unknown.
		o.mu.Lock()
		unknown := o.url == ""
		o.mu.Unlock()
		if unknown {
			o.reset("(unreadable)", "\x00unreadable")
		}
		failures = append(failures, "read the backup destination: "+err.Error())
	} else {
		if err := o.syncController(ctx, client); err != nil {
			failures = append(failures, "controller backups: "+err.Error())
		}
		volumeFailures, skipped := o.syncVolumes(ctx, client)
		failures, incomplete = append(failures, volumeFailures...), skipped
	}
	if ctx.Err() != nil {
		return
	}
	now := o.service.now().UTC()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(failures) == 0 {
		// A sync that left volumes out still clears the last failure, but
		// only a complete one says when every backup was last copied.
		o.failedAt, o.failure = time.Time{}, ""
		if !incomplete {
			o.syncedAt = now
		}
		return
	}
	o.failedAt, o.failure = now, failures[0]
	if len(failures) > 1 {
		o.failure += fmt.Sprintf(" (and %d more)", len(failures)-1)
	}
	log.Printf("controller: copy backups to the bucket: %s", strings.Join(failures, "; "))
}

// reset forgets the status of the previous destination when identity
// names another one.
func (o *offsiteBackups) reset(url, identity string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if identity == o.identity {
		return
	}
	o.url, o.identity, o.syncedAt, o.failedAt, o.failure = url, identity, time.Time{}, time.Time{}, ""
	clear(o.failing)
}

// syncController uploads the newest keep controller backup sets the bucket
// does not have yet and prunes the rest there.
func (o *offsiteBackups) syncController(ctx context.Context, client *objectstore.Client) error {
	if o.dataDir == "" {
		return nil
	}
	sets, err := clusterctl.ListBackupSets(o.dataDir)
	if err != nil {
		return err
	}
	prefix := client.Destination().Key(objectstore.ControllerBackups)
	remote, err := listArchives(ctx, client, prefix)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(sets))
	for _, set := range sets {
		names = append(names, filepath.Base(set.Path))
	}
	for _, index := range missingNewest(names, remote, o.keep) {
		name := names[index]
		if err := withTimeout(ctx, offsiteTransferLimit, func(ctx context.Context) error {
			return uploadBackupSet(ctx, client, sets[index].Path, prefix+name+objectstore.ArchiveSuffix)
		}); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
		remote[name] = true
	}
	return pruneRemote(ctx, client, prefix, remote, o.keep, time.Time{})
}

// missingNewest returns the indexes, oldest first, of the backups (named
// newest first) the bucket does not have yet. Scheduled backups older than
// the newest keep are left out, because pruning would delete them again;
// labeled ones are always included. keep 0 includes every backup.
func missingNewest(names []string, remote map[string]bool, keep int) []int {
	var missing []int
	scheduled := 0
	for index, name := range names {
		if isScheduledBackup(name) {
			scheduled++
			if keep > 0 && scheduled > keep {
				continue
			}
		}
		if !remote[name] {
			missing = append(missing, index)
		}
	}
	for left, right := 0, len(missing)-1; left < right; left, right = left+1, right-1 {
		missing[left], missing[right] = missing[right], missing[left]
	}
	return missing
}

// uploadBackupSet archives a controller backup set and uploads it: one PUT
// up to 1 GiB, a multipart upload above.
func uploadBackupSet(ctx context.Context, client *objectstore.Client, directory, key string) error {
	staged, err := os.CreateTemp(filepath.Dir(directory), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if err := clusterctl.ArchiveBackupSet(directory, contextWriter{ctx, staged}); err != nil {
		return err
	}
	size, err := staged.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if size <= offsiteSinglePutLimit {
		return client.Put(ctx, key, io.NewSectionReader(staged, 0, size), size)
	}
	partSize := int64(offsitePartSize(uint64(size)))
	uploadID, err := createMultipart(ctx, client, key)
	if err != nil {
		return err
	}
	var etags []string
	for offset := int64(0); offset < size; offset += partSize {
		length := min(partSize, size-offset)
		var etag string
		err := retry(ctx, func() error {
			var err error
			etag, err = client.UploadPart(ctx, key, uploadID, len(etags)+1, io.NewSectionReader(staged, offset, length), length)
			return err
		})
		if err != nil {
			abortMultipart(ctx, client, key, uploadID)
			return err
		}
		etags = append(etags, etag)
	}
	return completeMultipart(ctx, client, key, uploadID, etags)
}

// contextWriter stops a long archive write once ctx ends.
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

// retry runs call up to three times, waiting a little longer each time, so
// one dropped connection does not restart a large upload.
func retry(ctx context.Context, call func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(attempt) * offsiteRetryDelay):
			}
		}
		if err = call(); err == nil || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// offsiteRetryDelay grows with each retry; tests shorten it.
var offsiteRetryDelay = 2 * time.Second

// offsitePartSize splits size into at most offsiteMaxParts parts of at
// least offsiteMinPartSize, rounded up to whole MiB.
func offsitePartSize(size uint64) uint64 {
	partSize := uint64(offsiteMinPartSize)
	if needed := (size + offsiteMaxParts - 1) / offsiteMaxParts; needed > partSize {
		partSize = (needed + 1<<20 - 1) &^ (1<<20 - 1)
	}
	return partSize
}

func createMultipart(ctx context.Context, client *objectstore.Client, key string) (string, error) {
	return timed(ctx, offsiteRequestTimeout, func(ctx context.Context) (string, error) { return client.CreateMultipartUpload(ctx, key) })
}

func completeMultipart(ctx context.Context, client *objectstore.Client, key, uploadID string, etags []string) error {
	if err := withTimeout(ctx, offsiteRequestTimeout, func(ctx context.Context) error {
		return client.CompleteMultipartUpload(ctx, key, uploadID, etags)
	}); err != nil {
		abortMultipart(ctx, client, key, uploadID)
		return err
	}
	return nil
}

// abortMultipart drops the uploaded parts, even when ctx has ended, but
// never waits long for it: buckets also expire unfinished uploads.
func abortMultipart(ctx context.Context, client *objectstore.Client, key, uploadID string) {
	_ = withTimeout(context.WithoutCancel(ctx), offsiteRequestTimeout/2, func(ctx context.Context) error {
		return client.AbortMultipartUpload(ctx, key, uploadID)
	})
}

// syncVolumes uploads the backups of every placed volume, scheduled or
// manual, using one listing of the bucket. A volume with a backup policy
// whose node cannot upload right now is reported, so status never claims a
// copy that is missing, but raises no notification.
func (o *offsiteBackups) syncVolumes(ctx context.Context, client *objectstore.Client) (failures []string, incomplete bool) {
	applications, err := o.service.state.Applications(ctx, false)
	if err != nil {
		return []string{"read applications: " + err.Error()}, false
	}
	remote, err := remoteBackups(ctx, client, "", "", offsiteRequestTimeout)
	if err != nil {
		return []string{"list volume backups in the bucket: " + err.Error()}, false
	}
	settled := o.service.now().Sub(o.startedAt) >= offsiteStartupGrace
	for _, application := range applications {
		targets, err := o.service.volumeTargets(ctx, application, "")
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", application.Name, err))
			continue
		}
		for _, target := range targets {
			if ctx.Err() != nil {
				return failures, incomplete
			}
			if !target.placed {
				continue
			}
			name := target.application + "/" + target.volume
			connected := o.service.sessions != nil && o.service.sessions.Connected(target.node.ID)
			if !connected || !o.service.sessions.offsiteBackups(target.node.ID) {
				// Right after a start the volume is only left out, so the sync
				// is not reported as complete. A volume without a policy has
				// only manual backups, so it is never reported.
				switch {
				case !connected && !settled:
					incomplete = true
				case target.backup == nil:
				case !connected:
					failures = append(failures, fmt.Sprintf("volume %s waits for node %s to connect", name, target.node.Name))
				case connected:
					failures = append(failures, fmt.Sprintf("volume %s waits for node %s to upgrade its agent", name, target.node.Name))
				}
				continue
			}
			inBucket := make(map[string]bool)
			for _, backup := range remote[name] {
				inBucket[backup.GetName()] = true
			}
			err := o.syncVolume(ctx, client, target, inBucket)
			o.recordVolume(ctx, target, err)
			if err != nil {
				failures = append(failures, fmt.Sprintf("volume %s: %v", name, err))
			}
		}
	}
	return failures, incomplete
}

func (o *offsiteBackups) recordVolume(ctx context.Context, target volumeTarget, err error) {
	if ctx.Err() != nil {
		// An interrupted sync says nothing about the volume.
		return
	}
	key := target.application + "/" + target.volume
	o.mu.Lock()
	wasFailing := o.failing[key]
	if err == nil {
		delete(o.failing, key)
	} else {
		o.failing[key] = true
	}
	o.mu.Unlock()
	if err == nil || wasFailing {
		return
	}
	message := boundedFailure("copy to the bucket failed: " + err.Error())
	if recordErr := o.service.state.RecordVolumeBackupUploadFailed(ctx, target.application, target.volume, target.node.ID,
		message, o.service.now()); recordErr != nil {
		log.Printf("controller: record backup upload failure: %v", recordErr)
	}
}

// boundedFailure cuts message to 512 bytes without splitting a character.
func boundedFailure(message string) string {
	if len(message) > 512 {
		message = message[:512]
	}
	return strings.ToValidUTF8(message, "")
}

// syncVolume uploads a volume's backups the bucket does not have and, for
// a volume with a backup policy, prunes the bucket to its keep.
func (o *offsiteBackups) syncVolume(ctx context.Context, client *objectstore.Client, target volumeTarget, remote map[string]bool) error {
	local, err := o.service.localVolumeBackups(ctx, target)
	if err != nil {
		return err
	}
	keep := 0
	if target.backup != nil {
		keep = target.backup.Keep
	}
	prefix := offsiteVolumePrefix(client.Destination(), target)
	names := make([]string, 0, len(local))
	for _, backup := range local {
		names = append(names, backup.GetName())
	}
	for _, index := range missingNewest(names, remote, keep) {
		backup := local[index]
		key := prefix + backup.GetName() + objectstore.ArchiveSuffix
		err := o.service.uploadVolumeBackup(ctx, client, target, backup, key)
		if connect.CodeOf(err) == connect.CodeNotFound {
			// The node pruned it since it listed it; nothing to copy.
			continue
		}
		if err != nil {
			return fmt.Errorf("upload %s: %w", backup.GetName(), err)
		}
		remote[backup.GetName()] = true
	}
	// Backups older than the volume's claim on this node were taken on a
	// node that was lost; pruning leaves them, so they stay restorable.
	since, err := o.service.state.VolumeClaimedAt(ctx, target.application, target.volume)
	if err != nil {
		return err
	}
	return pruneRemote(ctx, client, prefix, remote, keep, since)
}

func offsiteVolumePrefix(destination objectstore.Destination, target volumeTarget) string {
	return destination.Key(objectstore.VolumeBackupsPrefix(target.application, target.volume))
}

// localVolumeBackups asks the volume's node for its backups, newest first.
func (s *service) localVolumeBackups(ctx context.Context, target volumeTarget) ([]*orchv1alpha1.VolumeBackupInfo, error) {
	var backups []*orchv1alpha1.VolumeBackupInfo
	err := withTimeout(ctx, offsiteRequestTimeout, func(ctx context.Context) error {
		return s.volumeOperation(ctx, target, &orchv1alpha1.VolumeCommand{Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_LIST},
			func(response *orchv1alpha1.VolumeResponse) error {
				backups = response.GetBackups()
				return nil
			})
	})
	return backups, err
}

// uploadVolumeBackup has the volume's node upload one backup through
// presigned URLs: one PUT up to 1 GiB, a multipart upload above.
func (s *service) uploadVolumeBackup(ctx context.Context, client *objectstore.Client, target volumeTarget, backup *orchv1alpha1.VolumeBackupInfo, key string) error {
	ctx, cancel := context.WithTimeout(ctx, offsiteTransferLimit)
	defer cancel()
	command := &orchv1alpha1.VolumeCommand{Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_UPLOAD, Backup: backup.GetName()}
	size := backup.GetSizeBytes()
	if size <= offsiteSinglePutLimit {
		url, err := client.PresignPut(key, offsitePresignExpiry)
		if err != nil {
			return err
		}
		command.Urls = []string{url}
		return s.volumeOperation(ctx, target, command, func(*orchv1alpha1.VolumeResponse) error { return nil })
	}
	partSize := offsitePartSize(size)
	parts := int((size + partSize - 1) / partSize)
	uploadID, err := createMultipart(ctx, client, key)
	if err != nil {
		return err
	}
	for part := 1; part <= parts; part++ {
		url, err := client.PresignUploadPart(key, uploadID, part, offsitePresignExpiry)
		if err != nil {
			abortMultipart(ctx, client, key, uploadID)
			return err
		}
		command.Urls = append(command.Urls, url)
	}
	command.PartSize = partSize
	var etags []string
	if err := s.volumeOperation(ctx, target, command, func(response *orchv1alpha1.VolumeResponse) error {
		etags = response.GetEtags()
		return nil
	}); err != nil {
		abortMultipart(ctx, client, key, uploadID)
		return err
	}
	if len(etags) != parts {
		abortMultipart(ctx, client, key, uploadID)
		return fmt.Errorf("node uploaded %d of %d parts", len(etags), parts)
	}
	return completeMultipart(ctx, client, key, uploadID, etags)
}

// pruneRemote keeps the newest keep scheduled backups below prefix taken
// since the given time. Labeled backups (manual, before-restore) stay, like
// on the node, and so do older ones.
func pruneRemote(ctx context.Context, client *objectstore.Client, prefix string, archives map[string]bool, keep int, since time.Time) error {
	if keep < 1 {
		return nil
	}
	since = since.Truncate(time.Second)
	var scheduled []string
	for name := range archives {
		if isScheduledBackup(name) && !objectstore.BackupTime(name).Before(since) {
			scheduled = append(scheduled, name)
		}
	}
	// Names are UTC timestamps, so they sort by age.
	sort.Sort(sort.Reverse(sort.StringSlice(scheduled)))
	for _, name := range scheduled[min(keep, len(scheduled)):] {
		if err := withTimeout(ctx, offsiteRequestTimeout, func(ctx context.Context) error {
			return client.Delete(ctx, prefix+name+objectstore.ArchiveSuffix)
		}); err != nil {
			return fmt.Errorf("remove old backup %s: %w", name, err)
		}
		delete(archives, name)
	}
	return nil
}

// remoteBackups lists every volume backup in the bucket below the
// application's prefix, or every application's when application is empty,
// keyed by application and volume, newest first. A volume narrows it to
// that volume's prefix.
func remoteBackups(ctx context.Context, client *objectstore.Client, application, volume string, limit time.Duration) (map[string][]*orchv1alpha1.VolumeBackupInfo, error) {
	base := client.Destination().Key(objectstore.VolumeBackups)
	prefix := base
	if application != "" {
		prefix += application + "/"
		if volume != "" {
			prefix += volume + "/"
		}
	}
	objects, err := listObjects(ctx, client, prefix, limit)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]*orchv1alpha1.VolumeBackupInfo)
	for _, object := range objects {
		parts := strings.Split(strings.TrimPrefix(object.Key, base), "/")
		if len(parts) != 3 {
			continue
		}
		name, ok := strings.CutSuffix(parts[2], objectstore.ArchiveSuffix)
		if !ok || !objectstore.ValidBackupName(name) {
			continue
		}
		createdAt := objectstore.BackupTime(name)
		key := parts[0] + "/" + parts[1]
		result[key] = append(result[key], &orchv1alpha1.VolumeBackupInfo{Name: name, CreatedAt: timestamppb.New(createdAt),
			SizeBytes: uint64(max(object.Size, 0)), InBucket: true})
	}
	for _, backups := range result {
		sortBackups(backups)
	}
	return result, nil
}

func sortBackups(backups []*orchv1alpha1.VolumeBackupInfo) {
	sort.SliceStable(backups, func(i, j int) bool {
		left, right := backups[i].GetCreatedAt().AsTime(), backups[j].GetCreatedAt().AsTime()
		if left.Equal(right) {
			return backups[i].GetName() > backups[j].GetName()
		}
		return left.After(right)
	})
}

// mergeVolumeBackups marks which local backups are in the bucket and adds
// the ones only the bucket has.
func mergeVolumeBackups(local, remote []*orchv1alpha1.VolumeBackupInfo) []*orchv1alpha1.VolumeBackupInfo {
	inBucket := make(map[string]bool, len(remote))
	for _, backup := range remote {
		inBucket[backup.GetName()] = true
	}
	merged := make([]*orchv1alpha1.VolumeBackupInfo, 0, len(local)+len(remote))
	onNode := make(map[string]bool, len(local))
	for _, backup := range local {
		onNode[backup.GetName()] = true
		backup.OnNode, backup.InBucket = true, inBucket[backup.GetName()]
		merged = append(merged, backup)
	}
	for _, backup := range remote {
		if !onNode[backup.GetName()] {
			merged = append(merged, backup)
		}
	}
	sortBackups(merged)
	return merged
}

// bucketSource returns a presigned URL for a volume backup the bucket has,
// or "" when there is no destination or the bucket does not have it. A
// bucket it cannot read, or an agent too old to download, is an error, so a
// restore reports that instead of a missing backup.
func (s *service) bucketSource(ctx context.Context, target volumeTarget, name string) (string, error) {
	client, err := s.offsiteClient(ctx)
	if err != nil {
		return "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the backup destination: %w", err))
	}
	if client == nil || !objectstore.ValidBackupName(name) {
		return "", nil
	}
	key := offsiteVolumePrefix(client.Destination(), target) + name + objectstore.ArchiveSuffix
	exists, err := timed(ctx, offsiteRequestTimeout, func(ctx context.Context) (bool, error) { return client.Exists(ctx, key) })
	if err != nil {
		return "", connect.NewError(connect.CodeUnavailable, fmt.Errorf("look for the backup in the bucket: %w", err))
	}
	if !exists {
		return "", nil
	}
	if !s.sessions.offsiteBackups(target.node.ID) {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("backup %s is only in the bucket; upgrade the agent on node %s to restore it", name, target.node.Name))
	}
	url, err := client.PresignGet(key, offsitePresignExpiry)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("presign the bucket's backup: %w", err))
	}
	return url, nil
}

// openBucketBackup opens a volume backup in the bucket for streaming to a
// client, or returns nil when there is no destination or no such backup.
func (s *service) openBucketBackup(ctx context.Context, target volumeTarget, name string) (io.ReadCloser, int64, error) {
	client, err := s.offsiteClient(ctx)
	if err != nil {
		return nil, 0, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the backup destination: %w", err))
	}
	if client == nil || !objectstore.ValidBackupName(name) {
		return nil, 0, nil
	}
	key := offsiteVolumePrefix(client.Destination(), target) + name + objectstore.ArchiveSuffix
	body, size, err := client.Get(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the backup from the bucket: %w", err))
	}
	if size < 0 {
		// Clients check a download against its size, so it must be known.
		body.Close()
		return nil, 0, connect.NewError(connect.CodeUnavailable, errors.New("the bucket did not say how large the backup is"))
	}
	return body, size, nil
}
