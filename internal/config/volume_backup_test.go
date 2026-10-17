package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLoadVolumeBackup(t *testing.T) {
	source := strings.Replace(cronMountManifest, `volumes { ["sqld"] {}; ["cache"] {}; ["spare"] {} }`, `volumes {
  ["sqld"] { backup = new VolumeBackup { before { "sqld-flush" } } }
  ["cache"] { backup = new VolumeBackup { schedule = "0 */6 * * *"; keep = 3 } }
  ["spare"] {}
}`, 1)
	manifest := loadManifest(t, source)
	if got, want := manifest.Volumes["sqld"].Backup, (&VolumeBackup{Schedule: "0 3 * * *", Keep: 7, Before: []string{"sqld-flush"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sqld backup = %#v", got)
	}
	if got := manifest.Volumes["cache"].Backup; got == nil || got.Schedule != "0 */6 * * *" || got.Keep != 3 || len(got.Before) != 0 {
		t.Fatalf("cache backup = %#v", got)
	}
	if manifest.Volumes["spare"].Backup != nil {
		t.Fatal("a volume without a backup block has a backup")
	}
	unset, err := CanonicalJSON(loadManifest(t, cronMountManifest))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unset), `"backup":`) {
		t.Fatalf("volumes without backups changed the canonical manifest: %s", unset)
	}
}

func TestValidateRejectsInvalidVolumeBackup(t *testing.T) {
	for name, test := range map[string]struct {
		volume string
		backup VolumeBackup
		path   string
	}{
		"unmounted volume": {"spare", VolumeBackup{Schedule: "0 3 * * *", Keep: 7}, "volumes.spare.backup"},
		"bad schedule":     {"sqld", VolumeBackup{Schedule: "@daily", Keep: 7}, "volumes.sqld.backup.schedule"},
		"keep zero":        {"sqld", VolumeBackup{Schedule: "0 3 * * *"}, "volumes.sqld.backup.keep"},
		"empty before":     {"sqld", VolumeBackup{Schedule: "0 3 * * *", Keep: 7, Before: []string{" "}}, "volumes.sqld.backup.before[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := loadManifest(t, cronMountManifest)
			volume := manifest.Volumes[test.volume]
			volume.Backup = &test.backup
			manifest.Volumes[test.volume] = volume
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v", validation)
			}
		})
	}
}
