package control

import (
	"context"
	"errors"
	"fmt"
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
	offsiteUploadTimeout  = 6 * time.Hour
	offsiteListTimeout    = 30 * time.Second
	offsiteSinglePutLimit = 1 << 30
	offsiteMinPartSize    = 128 << 20
	offsiteMaxParts       = 1000
	// Object keys below the destination prefix.
	offsiteControllerKey = "controller/"
	offsiteVolumesKey    = "volumes/"
	offsiteMarkerKey     = "orch-cluster"
	offsiteArchiveSuffix = ".tar.gz"
)

// offsiteBackups copies every backup to the cluster's S3 bucket, when one is
// set. The controller keeps the bucket's credentials: it uploads its own
// backup sets itself and hands workers presigned URLs for their volume
// backups. A sync runs every 15 minutes and right after a manual backup, and
// uploads whatever the bucket is missing, so a failed upload is retried on
// the next run.
type offsiteBackups struct {
	service *service
	// dataDir holds the controller's backup sets; keep is how many of them
	// the bucket keeps. Both are zero when controller backups are off.
	dataDir string
	keep    int
	wake    chan struct{}

	mu sync.Mutex
	// url is the destination of the last sync, for status.
	url      string
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
	return &offsiteBackups{service: service, dataDir: dataDir, keep: keep, wake: make(chan struct{}, 1), failing: make(map[string]bool)}
}

func (o *offsiteBackups) status() offsiteStatus {
	if o == nil {
		return offsiteStatus{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return offsiteStatus{URL: o.url, SyncedAt: o.syncedAt, FailedAt: o.failedAt, Failure: o.failure}
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
		o.syncOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-o.wake:
		}
	}
}

// client returns a client for the stored destination, or nil when none is
// set.
func (s *service) offsiteClient(ctx context.Context) (*objectstore.Client, error) {
	destination, found, err := s.state.BackupDestination(ctx, s.secretBox)
	if err != nil || !found {
		return nil, err
	}
	return objectstore.New(destination, nil), nil
}

func (o *offsiteBackups) syncOnce(ctx context.Context) {
	client, err := o.service.offsiteClient(ctx)
	if client == nil && err == nil {
		o.reset("")
		return
	}
	if client != nil && o.status().URL != client.Destination().URL() {
		o.reset(client.Destination().URL())
	}
	var failures []string
	if err != nil {
		failures = append(failures, err.Error())
	} else {
		if err := o.syncController(ctx, client); err != nil {
			failures = append(failures, "controller backups: "+err.Error())
		}
		failures = append(failures, o.syncVolumes(ctx, client)...)
	}
	if ctx.Err() != nil {
		return
	}
	now := o.service.now().UTC()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(failures) == 0 {
		o.syncedAt, o.failedAt, o.failure = now, time.Time{}, ""
		return
	}
	o.failedAt, o.failure = now, failures[0]
	if len(failures) > 1 {
		o.failure += fmt.Sprintf(" (and %d more)", len(failures)-1)
	}
	log.Printf("controller: copy backups to the bucket: %s", strings.Join(failures, "; "))
}

// reset forgets the status of the previous destination.
func (o *offsiteBackups) reset(url string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.url, o.syncedAt, o.failedAt, o.failure = url, time.Time{}, time.Time{}, ""
	clear(o.failing)
}

// syncController uploads the controller's backup sets the bucket does not
// have yet and keeps the newest keep there.
func (o *offsiteBackups) syncController(ctx context.Context, client *objectstore.Client) error {
	if o.dataDir == "" {
		return nil
	}
	sets, err := clusterctl.ListBackupSets(o.dataDir)
	if err != nil {
		return err
	}
	destination := client.Destination()
	remote, err := remoteBackupNames(ctx, client, destination.Key(offsiteControllerKey))
	if err != nil {
		return err
	}
	for index := len(sets) - 1; index >= 0; index-- {
		name := filepath.Base(sets[index].Path)
		if remote[name] {
			continue
		}
		if err := uploadBackupSet(ctx, client, sets[index].Path, destination.Key(offsiteControllerKey+name+offsiteArchiveSuffix)); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
		remote[name] = true
	}
	return pruneRemote(ctx, client, destination.Key(offsiteControllerKey), remote, o.keep)
}

func uploadBackupSet(ctx context.Context, client *objectstore.Client, directory, key string) error {
	staged, err := os.CreateTemp(filepath.Dir(directory), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if err := clusterctl.ArchiveBackupSet(directory, staged); err != nil {
		return err
	}
	size, err := staged.Seek(0, 1)
	if err != nil {
		return err
	}
	if _, err := staged.Seek(0, 0); err != nil {
		return err
	}
	return client.Put(ctx, key, staged, size)
}

// syncVolumes uploads the backups of every volume with a backup policy. A
// volume whose node is not connected is skipped until it is.
func (o *offsiteBackups) syncVolumes(ctx context.Context, client *objectstore.Client) []string {
	applications, err := o.service.state.Applications(ctx, false)
	if err != nil {
		return []string{"read applications: " + err.Error()}
	}
	var failures []string
	for _, application := range applications {
		targets, err := o.service.volumeTargets(ctx, application, "")
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", application.Name, err))
			continue
		}
		for _, target := range targets {
			if ctx.Err() != nil {
				return failures
			}
			if target.backup == nil || !target.placed || o.service.sessions == nil ||
				!o.service.sessions.Connected(target.node.ID) || !o.service.sessions.offsiteBackups(target.node.ID) {
				continue
			}
			err := o.syncVolume(ctx, client, target)
			o.recordVolume(ctx, target, err)
			if err != nil {
				failures = append(failures, fmt.Sprintf("volume %s/%s: %v", target.application, target.volume, err))
			}
		}
	}
	return failures
}

func (o *offsiteBackups) recordVolume(ctx context.Context, target volumeTarget, err error) {
	key := target.application + "/" + target.volume
	o.mu.Lock()
	wasFailing := o.failing[key]
	if err == nil {
		delete(o.failing, key)
	} else {
		o.failing[key] = true
	}
	o.mu.Unlock()
	if err == nil || wasFailing || ctx.Err() != nil {
		return
	}
	message := boundedFailure("copy to the bucket failed: " + err.Error())
	if recordErr := o.service.state.RecordVolumeBackupUploadFailed(ctx, target.application, target.volume, target.node.ID,
		message, o.service.now()); recordErr != nil {
		log.Printf("controller: record backup upload failure: %v", recordErr)
	}
}

func boundedFailure(message string) string {
	if len(message) > 512 {
		return message[:512]
	}
	return message
}

func (o *offsiteBackups) syncVolume(ctx context.Context, client *objectstore.Client, target volumeTarget) error {
	local, err := o.service.localVolumeBackups(ctx, target)
	if err != nil {
		return err
	}
	prefix := offsiteVolumePrefix(client.Destination(), target)
	remote, err := remoteBackupNames(ctx, client, prefix)
	if err != nil {
		return err
	}
	for index := len(local) - 1; index >= 0; index-- {
		backup := local[index]
		if remote[backup.GetName()] {
			continue
		}
		if err := o.service.uploadVolumeBackup(ctx, client, target, backup, prefix+backup.GetName()+offsiteArchiveSuffix); err != nil {
			return fmt.Errorf("upload %s: %w", backup.GetName(), err)
		}
		remote[backup.GetName()] = true
	}
	return pruneRemote(ctx, client, prefix, remote, target.backup.Keep)
}

func offsiteVolumePrefix(destination objectstore.Destination, target volumeTarget) string {
	return destination.Key(offsiteVolumesKey + target.application + "/" + target.volume + "/")
}

// localVolumeBackups asks the volume's node for its backups, newest first.
func (s *service) localVolumeBackups(ctx context.Context, target volumeTarget) ([]*orchv1alpha1.VolumeBackupInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, offsiteListTimeout)
	defer cancel()
	var backups []*orchv1alpha1.VolumeBackupInfo
	err := s.volumeOperation(ctx, target, &orchv1alpha1.VolumeCommand{Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_LIST},
		func(response *orchv1alpha1.VolumeResponse) error {
			backups = response.GetBackups()
			return nil
		})
	return backups, err
}

// uploadVolumeBackup has the volume's node upload one backup through
// presigned URLs: one PUT up to 1 GiB, a multipart upload above.
func (s *service) uploadVolumeBackup(ctx context.Context, client *objectstore.Client, target volumeTarget, backup *orchv1alpha1.VolumeBackupInfo, key string) error {
	ctx, cancel := context.WithTimeout(ctx, offsiteUploadTimeout)
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
	partSize := uint64(offsiteMinPartSize)
	if needed := (size + offsiteMaxParts - 1) / offsiteMaxParts; needed > partSize {
		partSize = (needed + 1<<20 - 1) &^ (1<<20 - 1)
	}
	parts := int((size + partSize - 1) / partSize)
	uploadID, err := client.CreateMultipartUpload(ctx, key)
	if err != nil {
		return err
	}
	abort := func() { _ = client.AbortMultipartUpload(context.WithoutCancel(ctx), key, uploadID) }
	for part := 1; part <= parts; part++ {
		url, err := client.PresignUploadPart(key, uploadID, part, offsitePresignExpiry)
		if err != nil {
			abort()
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
		abort()
		return err
	}
	if len(etags) != parts {
		abort()
		return fmt.Errorf("node uploaded %d of %d parts", len(etags), parts)
	}
	if err := client.CompleteMultipartUpload(ctx, key, uploadID, etags); err != nil {
		abort()
		return err
	}
	return nil
}

// remoteBackupNames lists the archives directly below prefix by backup name.
func remoteBackupNames(ctx context.Context, client *objectstore.Client, prefix string) (map[string]bool, error) {
	objects, err := client.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(objects))
	for _, object := range objects {
		name, ok := strings.CutSuffix(strings.TrimPrefix(object.Key, prefix), offsiteArchiveSuffix)
		if ok && name != "" && !strings.Contains(name, "/") {
			names[name] = true
		}
	}
	return names, nil
}

// pruneRemote keeps the newest keep scheduled backups below prefix. Labeled
// backups (manual, before-restore) stay, like on the node.
func pruneRemote(ctx context.Context, client *objectstore.Client, prefix string, names map[string]bool, keep int) error {
	if keep < 1 {
		return nil
	}
	var scheduled []string
	for name := range names {
		if !strings.Contains(name, "-") {
			scheduled = append(scheduled, name)
		}
	}
	// Names are UTC timestamps, so they sort by age.
	sort.Sort(sort.Reverse(sort.StringSlice(scheduled)))
	for _, name := range scheduled[min(keep, len(scheduled)):] {
		if err := client.Delete(ctx, prefix+name+offsiteArchiveSuffix); err != nil {
			return fmt.Errorf("remove old backup %s: %w", name, err)
		}
		delete(names, name)
	}
	return nil
}

// remoteVolumeBackups lists a volume's backups in the bucket, newest first.
func (s *service) remoteVolumeBackups(ctx context.Context, client *objectstore.Client, target volumeTarget) ([]*orchv1alpha1.VolumeBackupInfo, error) {
	prefix := offsiteVolumePrefix(client.Destination(), target)
	objects, err := client.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var backups []*orchv1alpha1.VolumeBackupInfo
	for _, object := range objects {
		name, ok := strings.CutSuffix(strings.TrimPrefix(object.Key, prefix), offsiteArchiveSuffix)
		if !ok || strings.Contains(name, "/") || len(name) < len(backupNameLayout) {
			continue
		}
		createdAt, err := time.Parse(backupNameLayout, name[:len(backupNameLayout)])
		if err != nil {
			continue
		}
		backups = append(backups, &orchv1alpha1.VolumeBackupInfo{Name: name, CreatedAt: timestamppb.New(createdAt),
			SizeBytes: uint64(max(object.Size, 0)), InBucket: true})
	}
	sortBackups(backups)
	return backups, nil
}

// backupNameLayout starts every volume backup name.
const backupNameLayout = "20060102T150405Z"

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

// fetchMissingBackup downloads a backup from the bucket onto the volume's
// node when the node does not have it, for example on a new node after the
// old one was lost. Without a destination, or when the bucket does not have
// it either, the restore reports the missing backup as before.
func (s *service) fetchMissingBackup(ctx context.Context, target volumeTarget, name string) error {
	client, err := s.offsiteClient(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("read backup destination"))
	}
	if client == nil || !s.sessions.offsiteBackups(target.node.ID) {
		return nil
	}
	local, err := s.localVolumeBackups(ctx, target)
	if err != nil {
		return err
	}
	for _, backup := range local {
		if backup.GetName() == name {
			return nil
		}
	}
	key := offsiteVolumePrefix(client.Destination(), target) + name + offsiteArchiveSuffix
	remote, err := s.remoteVolumeBackups(ctx, client, target)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("list backups in the bucket: %w", err))
	}
	found := false
	for _, backup := range remote {
		found = found || backup.GetName() == name
	}
	if !found {
		return nil
	}
	url, err := client.PresignGet(key, offsitePresignExpiry)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	return s.volumeOperation(ctx, target, &orchv1alpha1.VolumeCommand{
		Action: orchv1alpha1.VolumeAction_VOLUME_ACTION_FETCH, Backup: name, Urls: []string{url},
	}, func(*orchv1alpha1.VolumeResponse) error { return nil })
}
