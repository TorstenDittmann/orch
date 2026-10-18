package config

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

// OverrideImages replaces workload images for one deployment. Each override is
// either NAME=REF, which targets the service or cron job called NAME, or a bare
// REF, which targets every workload whose image has the same repository. Every
// override must match at least one workload.
func OverrideImages(manifest *Manifest, overrides []string) error {
	if len(overrides) == 0 {
		return nil
	}
	replaced := map[string]string{}
	set := func(kind, name, image string) error {
		key := kind + "." + name
		if previous, ok := replaced[key]; ok && previous != image {
			return fmt.Errorf("%s %q has conflicting image overrides", kind, name)
		}
		replaced[key] = image
		return nil
	}
	for _, override := range overrides {
		name, image, named := strings.Cut(override, "=")
		if !named {
			image, name = name, ""
		}
		parsed, err := reference.ParseNormalizedNamed(image)
		if err != nil || len(image) > 1024 {
			return fmt.Errorf("image override %q is not a valid OCI image reference", override)
		}
		matched := false
		for _, serviceName := range sortedKeys(manifest.Services) {
			if serviceName == name || (!named && sameRepository(manifest.Services[serviceName].Image, parsed)) {
				if err := set("service", serviceName, image); err != nil {
					return err
				}
				matched = true
			}
		}
		for _, jobName := range sortedKeys(manifest.CronJobs) {
			if jobName == name || (!named && sameRepository(manifest.CronJobs[jobName].Image, parsed)) {
				if err := set("cron job", jobName, image); err != nil {
					return err
				}
				matched = true
			}
		}
		if !matched && named {
			return fmt.Errorf("image override %q names no service or cron job", override)
		}
		if !matched {
			return fmt.Errorf("image override %q matches no service or cron job using %s", override, parsed.Name())
		}
	}
	for key, image := range replaced {
		kind, name, _ := strings.Cut(key, ".")
		if kind == "service" {
			service := manifest.Services[name]
			service.Image = image
			manifest.Services[name] = service
		} else {
			job := manifest.CronJobs[name]
			job.Image = image
			manifest.CronJobs[name] = job
		}
	}
	return Validate(manifest)
}

func sameRepository(image string, target reference.Named) bool {
	parsed, err := reference.ParseNormalizedNamed(image)
	return err == nil && parsed.Name() == target.Name()
}
