package objectstore

import "strings"

// Backups are stored below the destination prefix as
// controller/NAME.tar.gz and volumes/APP/VOLUME/NAME.tar.gz, where NAME
// starts with the backup's UTC time, so names sort by age. ClusterMarker
// holds the ID of the cluster that owns the prefix.
const (
	ControllerBackups = "controller/"
	VolumeBackups     = "volumes/"
	ClusterMarker     = "orch-cluster"
	ArchiveSuffix     = ".tar.gz"
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
		if ok && name != "" && !strings.Contains(name, "/") {
			archives[name] = object
		}
	}
	return archives
}
