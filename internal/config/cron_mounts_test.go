package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const cronMountManifest = `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "nmv"

services {
  ["db"] {
    image = "example.invalid/sqld:1"
    tcp = new Tcp { port = 8080 }
    mounts { new Mount { source = "sqld"; target = "/var/lib/sqld" } }
  }
  ["cache"] {
    image = "example.invalid/cache:1"
    mounts { new Mount { source = "cache"; target = "/cache" } }
  }
}

cronJobs {
  ["db-backup"] {
    image = "example.invalid/backup:1"
    schedule = "0 3 * * *"
    mounts { new Mount { source = "sqld"; target = "/sqld"; readOnly = true } }
  }
}

volumes { ["sqld"] {}; ["cache"] {}; ["spare"] {} }
`

func TestLoadCronJobReadOnlyMounts(t *testing.T) {
	manifest := loadManifest(t, cronMountManifest)
	want := []Mount{{Source: "sqld", Target: "/sqld", ReadOnly: true}}
	if got := manifest.CronJobs["db-backup"].Mounts; !reflect.DeepEqual(got, want) {
		t.Fatalf("cron mounts = %#v", got)
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.CronJobs["db-backup"].Mounts; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical cron mounts = %#v", got)
	}
}

func TestPklRejectsWritableCronMount(t *testing.T) {
	source := strings.Replace(cronMountManifest, `target = "/sqld"; readOnly = true`, `target = "/sqld"`, 1)
	if _, err := loadManifestError(t, source); err == nil {
		t.Fatal("writable cron mount was accepted")
	}
}

func TestValidateRejectsUnsafeCronMounts(t *testing.T) {
	cases := map[string]struct {
		mounts []Mount
		path   string
	}{
		"writable":         {[]Mount{{Source: "sqld", Target: "/sqld"}}, "cron_jobs.db-backup.mounts[0].read_only"},
		"undeclared":       {[]Mount{{Source: "missing", Target: "/sqld", ReadOnly: true}}, "cron_jobs.db-backup.mounts[0].source"},
		"unowned volume":   {[]Mount{{Source: "spare", Target: "/sqld", ReadOnly: true}}, "cron_jobs.db-backup.mounts[0].source"},
		"protected target": {[]Mount{{Source: "sqld", Target: "/proc/x", ReadOnly: true}}, "cron_jobs.db-backup.mounts[0].target"},
		"two owners": {[]Mount{
			{Source: "cache", Target: "/a", ReadOnly: true}, {Source: "sqld", Target: "/b", ReadOnly: true},
		}, "cron_jobs.db-backup.mounts[1].source"},
		"duplicate target": {[]Mount{
			{Source: "sqld", Target: "/a", ReadOnly: true}, {Source: "sqld", Target: "/a", ReadOnly: true},
		}, "cron_jobs.db-backup.mounts[1].target"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := loadManifest(t, cronMountManifest)
			job := manifest.CronJobs["db-backup"]
			job.Mounts = test.mounts
			manifest.CronJobs["db-backup"] = job
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v, want problem at %s", validation, test.path)
			}
		})
	}
}
