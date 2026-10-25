package ingress

import (
	"context"
	"errors"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
)

const RequiredCaddyVersion = "v2.11.3"

var ErrUnsupported = errors.New("ingress is unsupported on this platform")

type Route struct {
	Application string
	Service     string
	Domain      string
	HTTPS       bool
	Upstreams   []string
	// BasicAuth, when set, is checked before a request reaches an upstream.
	BasicAuth *model.HTTPBasicAuth
	// RedirectFrom lists domains that permanently redirect to Domain.
	RedirectFrom []string
}

type Controller interface {
	Preflight(context.Context) (string, error)
	Apply(context.Context, []Route, time.Time) ([]model.RouteStatus, error)
	Close() error
}

type Fake struct {
	Version    string
	Err        error
	Routes     []Route
	ApplyCalls int
}

func (f *Fake) Preflight(context.Context) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	if f.Version == "" {
		return RequiredCaddyVersion, nil
	}
	return f.Version, nil
}

func (f *Fake) Apply(_ context.Context, routes []Route, now time.Time) ([]model.RouteStatus, error) {
	f.ApplyCalls++
	f.Routes = cloneRoutes(routes)
	if f.Err != nil {
		return nil, f.Err
	}
	result := make([]model.RouteStatus, 0, len(routes))
	for _, route := range routes {
		result = append(result, model.RouteStatus{
			Application: route.Application, Service: route.Service, Domain: route.Domain,
			HTTPS: route.HTTPS, State: model.RouteActive, CertificateState: certificateState(route),
			UpstreamCount: uint32(len(route.Upstreams)), UpdatedAt: now.UTC(),
		})
	}
	return result, nil
}

func (*Fake) Close() error { return nil }

// certificateState names how a route's certificate is handled. Wildcard
// routes get one certificate per subdomain on its first HTTPS request.
func certificateState(route Route) string {
	switch {
	case !route.HTTPS:
		return "disabled"
	case config.IsWildcardDomain(route.Domain):
		return "on_demand"
	default:
		return "managed"
	}
}

func cloneRoutes(input []Route) []Route {
	result := make([]Route, len(input))
	for index, route := range input {
		result[index] = route
		result[index].Upstreams = append([]string(nil), route.Upstreams...)
		result[index].RedirectFrom = append([]string(nil), route.RedirectFrom...)
	}
	return result
}
