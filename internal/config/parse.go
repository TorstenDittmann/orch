package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Parse strictly decodes, resolves defaults, normalizes, and validates a
// manifest. Callers never receive a partially valid Manifest.
func Parse(data []byte) (*Manifest, error) {
	var doc document
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return nil, fmt.Errorf("decode manifest:\n%s", strict.String())
		}
		var decode *toml.DecodeError
		if errors.As(err, &decode) {
			return nil, fmt.Errorf("decode manifest:\n%s", decode.String())
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	manifest := resolve(doc)
	if err := Validate(manifest); err != nil {
		return nil, err
	}
	normalize(manifest)
	return manifest, nil
}

func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %q: %w", path, err)
	}
	manifest, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return manifest, nil
}

func resolve(doc document) *Manifest {
	deploy := Deployment{
		MaxSurge:       1,
		MaxUnavailable: 0,
		HealthDeadline: DefaultHealthDeadline,
		AutoRollback:   true,
	}
	if doc.Deploy.MaxSurge != nil {
		deploy.MaxSurge = *doc.Deploy.MaxSurge
	}
	if doc.Deploy.MaxUnavailable != nil {
		deploy.MaxUnavailable = *doc.Deploy.MaxUnavailable
	}
	if doc.Deploy.HealthDeadline != "" {
		deploy.HealthDeadline = doc.Deploy.HealthDeadline
	}
	if doc.Deploy.AutoRollback != nil {
		deploy.AutoRollback = *doc.Deploy.AutoRollback
	}

	manifest := &Manifest{
		Schema:   doc.Schema,
		Name:     doc.Name,
		Services: make(map[string]Service, len(doc.Services)),
		Volumes:  make(map[string]Volume, len(doc.Volumes)),
		Deploy:   deploy,
	}

	for name, input := range doc.Services {
		replicas := 1
		if input.Replicas != nil {
			replicas = *input.Replicas
		}
		restart := input.Restart
		if restart == "" {
			restart = DefaultRestart
		}
		cpu := input.Resources.CPU
		if cpu == "" {
			cpu = DefaultCPU
		}
		memory := input.Resources.Memory
		if memory == "" {
			memory = DefaultMemory
		}

		var httpConfig *HTTP
		if input.HTTP != nil {
			https := true
			if input.HTTP.HTTPS != nil {
				https = *input.HTTP.HTTPS
			}
			httpConfig = &HTTP{
				Port:       input.HTTP.Port,
				Domains:    append([]string(nil), input.HTTP.Domains...),
				HealthPath: input.HTTP.HealthPath,
				HTTPS:      https,
			}
		}

		manifest.Services[name] = Service{
			Image:       input.Image,
			Command:     append([]string(nil), input.Command...),
			Args:        append([]string(nil), input.Args...),
			Replicas:    replicas,
			Restart:     restart,
			Resources:   Resources{CPU: cpu, Memory: memory},
			HTTP:        httpConfig,
			Env:         cloneMap(input.Env),
			SecretEnv:   cloneMap(input.SecretEnv),
			SecretFiles: append([]SecretFile(nil), input.SecretFiles...),
			Constraints: cloneMap(input.Constraints),
			Mounts:      append([]Mount(nil), input.Mounts...),
		}
	}

	for name, input := range doc.Volumes {
		driver := input.Driver
		if driver == "" {
			driver = "local"
		}
		manifest.Volumes[name] = Volume{Driver: driver}
	}

	return manifest
}

func normalize(manifest *Manifest) {
	manifest.Name = strings.ToLower(manifest.Name)
	for name, service := range manifest.Services {
		service.Resources.CPU = normalizeCPU(service.Resources.CPU)
		if service.HTTP != nil {
			for i := range service.HTTP.Domains {
				service.HTTP.Domains[i] = strings.TrimSuffix(strings.ToLower(service.HTTP.Domains[i]), ".")
			}
			sort.Strings(service.HTTP.Domains)
		}
		sort.Slice(service.SecretFiles, func(i, j int) bool {
			return service.SecretFiles[i].Target < service.SecretFiles[j].Target
		})
		sort.Slice(service.Mounts, func(i, j int) bool {
			if service.Mounts[i].Target == service.Mounts[j].Target {
				return service.Mounts[i].Source < service.Mounts[j].Source
			}
			return service.Mounts[i].Target < service.Mounts[j].Target
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
