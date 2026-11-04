package config

import (
	"fmt"
	"slices"
	"strings"
)

// Preview turns manifest into the preview of pull request number: the
// application is renamed NAME-pr-N, every domain gets a pr-N. prefix, and
// the preview runs one replica of each service without backups,
// notifications or ports that only one application can own. It returns the
// preview's URLs, one per routed domain.
func Preview(manifest *Manifest, number int) ([]string, error) {
	if number <= 0 {
		return nil, fmt.Errorf("pull request number must be positive")
	}
	label := fmt.Sprintf("pr-%d", number)
	manifest.Name = manifest.Name + "-" + label
	if err := ValidateApplicationName(manifest.Name); err != nil {
		return nil, fmt.Errorf("preview name %s: %w", manifest.Name, err)
	}
	prefix := func(domain string) string {
		// A wildcard keeps its leading label so it still matches one level.
		if rest, ok := strings.CutPrefix(domain, "*."); ok {
			return "*." + label + "." + rest
		}
		return label + "." + domain
	}
	var urls []string
	for name, service := range manifest.Services {
		service.Replicas = 1
		service.Autoscale = nil
		if service.HTTP != nil {
			http := *service.HTTP
			// The public listener belongs to production.
			http.Public = false
			domains := make([]string, 0, len(http.Domains))
			for _, domain := range http.Domains {
				domains = append(domains, prefix(domain))
				if !strings.HasPrefix(domain, "*.") && http.Redirects[domain] == "" {
					scheme := "http"
					if http.HTTPS {
						scheme = "https"
					}
					urls = append(urls, scheme+"://"+prefix(domain))
				}
			}
			http.Domains = domains
			if len(http.Redirects) != 0 {
				redirects := make(map[string]string, len(http.Redirects))
				for from, to := range http.Redirects {
					redirects[prefix(from)] = prefix(to)
				}
				http.Redirects = redirects
			}
			if len(http.DomainPorts) != 0 {
				ports := make(map[string]int, len(http.DomainPorts))
				for domain, port := range http.DomainPorts {
					ports[prefix(domain)] = port
				}
				http.DomainPorts = ports
			}
			service.HTTP = &http
		}
		// Public and node ports are owned by one application cluster-wide.
		if service.TCP != nil {
			tcp := *service.TCP
			tcp.PublicPort, tcp.NodePort = nil, nil
			service.TCP = &tcp
		}
		service.UDP = nil
		manifest.Services[name] = service
	}
	for name, volume := range manifest.Volumes {
		volume.Backup = nil
		manifest.Volumes[name] = volume
	}
	manifest.Notifications = nil
	manifest.Deploy.AutoUpdate = false
	slices.Sort(urls)
	return slices.Compact(urls), nil
}
