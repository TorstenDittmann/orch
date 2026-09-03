package config

import (
	"sort"
	"strings"
)

func normalize(manifest *Manifest) {
	manifest.Name = strings.ToLower(manifest.Name)
	for name, service := range manifest.Services {
		service.Resources.CPU = normalizeCPU(service.Resources.CPU)
		if service.HTTP != nil {
			for index := range service.HTTP.Domains {
				service.HTTP.Domains[index] = strings.TrimSuffix(strings.ToLower(service.HTTP.Domains[index]), ".")
			}
			sort.Strings(service.HTTP.Domains)
		}
		sort.Slice(service.SecretFiles, func(left, right int) bool {
			return service.SecretFiles[left].Target < service.SecretFiles[right].Target
		})
		sort.Slice(service.Mounts, func(left, right int) bool {
			if service.Mounts[left].Target == service.Mounts[right].Target {
				return service.Mounts[left].Source < service.Mounts[right].Source
			}
			return service.Mounts[left].Target < service.Mounts[right].Target
		})
		service.Env = nilIfEmpty(service.Env)
		service.SecretEnv = nilIfEmpty(service.SecretEnv)
		service.Constraints = nilIfEmpty(service.Constraints)
		if len(service.Command) == 0 {
			service.Command = nil
		}
		if len(service.Args) == 0 {
			service.Args = nil
		}
		if len(service.SecretFiles) == 0 {
			service.SecretFiles = nil
		}
		if len(service.Mounts) == 0 {
			service.Mounts = nil
		}
		sort.Strings(service.DependsOn)
		if len(service.DependsOn) == 0 {
			service.DependsOn = nil
		}
		manifest.Services[name] = service
	}
	for name, job := range manifest.CronJobs {
		job.Resources.CPU = normalizeCPU(job.Resources.CPU)
		job.Env = nilIfEmpty(job.Env)
		sort.Strings(job.DependsOn)
		if len(job.Command) == 0 {
			job.Command = nil
		}
		if len(job.Args) == 0 {
			job.Args = nil
		}
		if len(job.DependsOn) == 0 {
			job.DependsOn = nil
		}
		manifest.CronJobs[name] = job
	}
	if len(manifest.Volumes) == 0 {
		manifest.Volumes = nil
	}
	if len(manifest.CronJobs) == 0 {
		manifest.CronJobs = nil
	}
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func nilIfEmpty(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	return input
}
