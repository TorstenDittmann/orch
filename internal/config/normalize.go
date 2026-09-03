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
		manifest.Services[name] = service
	}
	if len(manifest.Volumes) == 0 {
		manifest.Volumes = nil
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
