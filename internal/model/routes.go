package model

// RouteKey names one HTTP ingress route: a whole domain when path is empty,
// or a domain and a path prefix, such as example.com/api. Route statuses use
// it as their domain so routes that share a domain stay distinct.
func RouteKey(domain, path string) string { return domain + path }

// RoutePaths returns the prefixes served on each domain; the empty prefix
// stands for the whole domain.
func RoutePaths(paths []string) []string {
	if len(paths) == 0 {
		return []string{""}
	}
	return paths
}

// RouteKeys returns the route of each domain with each path, in order.
func RouteKeys(domains, paths []string) []string {
	keys := make([]string, 0, len(domains)*len(RoutePaths(paths)))
	for _, domain := range domains {
		for _, path := range RoutePaths(paths) {
			keys = append(keys, RouteKey(domain, path))
		}
	}
	return keys
}
