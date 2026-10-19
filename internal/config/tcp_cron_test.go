package config

import (
	"errors"
	"reflect"
	"testing"
)

const tcpCronManifest = `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "platform"

services {
  ["gitd"] {
    image = "example.invalid/gitd:1"
    constraints {
      ["storage"] = "fast"
    }
    tcp = new Tcp {
      port = 9418
      publicPort = 9418
    }
  }
  ["web"] {
    image = "example.invalid/web:1"
    dependsOn {
      "redis"
      "gitd"
    }
    http = new Http { port = 8080 }
  }
  ["redis"] {
    image = "example.invalid/redis:1"
    tcp = new Tcp { port = 6379 }
  }
}

cronJobs {
  ["backup-monitor"] {
    image = "example.invalid/backup:1"
    schedule = "*/2 * * * *"
    dependsOn { "gitd" }
    constraints {
      ["region"] = "eu-west"
    }
  }
}
`

func TestLoadTypedTCPDependenciesAndCronDefaults(t *testing.T) {
	manifest := loadManifest(t, tcpCronManifest)
	gitd := manifest.Services["gitd"]
	if gitd.TCP == nil || gitd.TCP.Port != 9418 || gitd.TCP.PublicPort == nil || *gitd.TCP.PublicPort != 9418 {
		t.Fatalf("gitd TCP = %#v", gitd.TCP)
	}
	if !reflect.DeepEqual(gitd.Constraints, map[string]string{"storage": "fast"}) {
		t.Fatalf("service constraints = %#v", gitd.Constraints)
	}
	if got := manifest.Services["web"].DependsOn; !reflect.DeepEqual(got, []string{"gitd", "redis"}) {
		t.Fatalf("depends_on = %#v", got)
	}
	job := manifest.CronJobs["backup-monitor"]
	if job.StartDeadline != DefaultJobStartDeadline || job.Timeout != DefaultJobTimeout || job.Resources.CPU != DefaultCPU {
		t.Fatalf("cron defaults = %#v", job)
	}
	if !reflect.DeepEqual(job.Constraints, map[string]string{"region": "eu-west"}) {
		t.Fatalf("cron constraints = %#v", job.Constraints)
	}
}

func TestCronConstraintsRoundTripCanonicalJSON(t *testing.T) {
	manifest := loadManifest(t, tcpCronManifest)
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.CronJobs["backup-monitor"].Constraints, map[string]string{"region": "eu-west"}) {
		t.Fatalf("cron constraints = %#v", parsed.CronJobs["backup-monitor"].Constraints)
	}
	if !reflect.DeepEqual(parsed.Services["gitd"].Constraints, map[string]string{"storage": "fast"}) {
		t.Fatalf("service constraints = %#v", parsed.Services["gitd"].Constraints)
	}
}

func TestValidateRejectsInvalidCronConstraints(t *testing.T) {
	manifest := loadManifest(t, tcpCronManifest)
	job := manifest.CronJobs["backup-monitor"]
	job.Constraints = map[string]string{"region": ""}
	manifest.CronJobs["backup-monitor"] = job
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "cron_jobs.backup-monitor.constraints") {
		t.Fatalf("problems = %#v", validation.Problems)
	}
}

func TestValidateRejectsTCPAndDependencyConflicts(t *testing.T) {
	manifest := loadManifest(t, tcpCronManifest)
	web := manifest.Services["web"]
	port := 9418
	web.TCP = &TCP{Port: 9000, PublicPort: &port}
	web.DependsOn = append(web.DependsOn, "missing")
	manifest.Services["web"] = web
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) {
		t.Fatalf("Validate() = %v", validation)
	}
	for _, path := range []string{"services.web", "services.web.tcp.public_port", "services.web.depends_on[2]"} {
		if !hasProblem(validation, path) {
			t.Errorf("problems = %#v, want %s", validation.Problems, path)
		}
	}
}

func TestValidateRejectsDependencyCycle(t *testing.T) {
	manifest := loadManifest(t, tcpCronManifest)
	gitd := manifest.Services["gitd"]
	gitd.DependsOn = []string{"web"}
	manifest.Services["gitd"] = gitd
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) {
		t.Fatalf("Validate() = %v", validation)
	}
	found := false
	for _, problem := range validation.Problems {
		if problem.Message == "creates a dependency cycle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("problems = %#v, want dependency cycle", validation.Problems)
	}
}

func TestValidateRejectsReservedTCPPortAndCronExtensions(t *testing.T) {
	manifest := loadManifest(t, tcpCronManifest)
	gitd := manifest.Services["gitd"]
	reserved := 443
	gitd.TCP.PublicPort = &reserved
	manifest.Services["gitd"] = gitd
	job := manifest.CronJobs["backup-monitor"]
	job.Schedule = "@hourly"
	manifest.CronJobs["backup-monitor"] = job
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) ||
		!hasProblem(validation, "services.gitd.tcp.public_port") ||
		!hasProblem(validation, "cron_jobs.backup-monitor.schedule") {
		t.Fatalf("problems = %#v", validation.Problems)
	}
}

func TestValidateRejectsCronScheduleThatNeverFires(t *testing.T) {
	manifest := loadManifest(t, tcpCronManifest)
	job := manifest.CronJobs["backup-monitor"]
	job.Schedule = "0 0 30 2 *"
	manifest.CronJobs["backup-monitor"] = job
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "cron_jobs.backup-monitor.schedule") {
		t.Fatalf("Validate() = %v", validation)
	}
}
