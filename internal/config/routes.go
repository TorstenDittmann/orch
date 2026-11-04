package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/TorstenDittmann/orch/internal/model"
)

// maxPathsPerService bounds http.paths, like the schema.
const maxPathsPerService = 20

// routePathPattern accepts prefixes such as /api or /v1/admin: whole
// segments without wildcards, Caddy placeholders, or a trailing slash.
// Caddy matches paths case-insensitively, so prefixes are lowercase.
var routePathPattern = regexp.MustCompile(`^(/[a-z0-9._~-]+)+$`)

// ValidateRoutePath reports whether path can be an http.paths prefix.
func ValidateRoutePath(path string) error {
	if len(path) > 256 || !routePathPattern.MatchString(path) {
		return fmt.Errorf("must be a path prefix such as /api: segments of lowercase letters, digits, '.', '_', '~' or '-', with no trailing slash")
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("must not contain . or .. segments")
		}
	}
	return nil
}

// RouteKeys returns every route a service claims on ingress, sorted: each
// domain with each path, and each domain that redirects to them.
func (h *HTTP) RouteKeys() []string {
	keys := model.RouteKeys(h.Domains, h.Paths)
	for from := range h.Redirects {
		keys = append(keys, from)
	}
	sort.Strings(keys)
	return keys
}
