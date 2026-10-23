package objectstore

import (
	"strings"
	"time"
)

// Backups are stored below the destination prefix as
// controller/NAME.tar.gz and volumes/APP/VOLUME/NAME.tar.gz, where NAME
// starts with the backup's UTC time, so names sort by age. ClusterMarker
// holds the ID of the cluster that owns the prefix.
const (
	ControllerBackups = "controller/"
	VolumeBackups     = "volumes/"
	ClusterMarker     = "orch-cluster"
	ArchiveSuffix     = ".tar.gz"
	// BackupNameLayout starts every backup name.
	BackupNameLayout = "20060102T150405Z"
)

// VolumeBackupsPrefix is where a volume's archives are, below the
// destination prefix.
func VolumeBackupsPrefix(application, volume string) string {
	return VolumeBackups + application + "/" + volume + "/"
}

// Archives returns the archives directly below prefix by backup name.
func Archives(objects []Object, prefix string) map[string]Object {
	archives := make(map[string]Object, len(objects))
	for _, object := range objects {
		rest, ok := strings.CutPrefix(object.Key, prefix)
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(rest, ArchiveSuffix)
		if ok && ValidBackupName(name) {
			archives[name] = object
		}
	}
	return archives
}

// ValidBackupName reports whether name is a backup name: a UTC time,
// optionally followed by a label of lowercase letters and hyphens. Other
// objects below the prefix are never treated as backups, so they are never
// pruned or restored.
func ValidBackupName(name string) bool {
	if len(name) < len(BackupNameLayout) {
		return false
	}
	if _, err := time.Parse(BackupNameLayout, name[:len(BackupNameLayout)]); err != nil {
		return false
	}
	label := name[len(BackupNameLayout):]
	if label == "" {
		return true
	}
	if label[0] != '-' || len(label) == 1 || len(label) > 32 {
		return false
	}
	for _, character := range label[1:] {
		if (character < 'a' || character > 'z') && character != '-' {
			return false
		}
	}
	return true
}
