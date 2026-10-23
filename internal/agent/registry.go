package agent

import (
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// controllerRegistry gives the runtime an HTTP client for the controller's
// image endpoint that always presents the agent's current certificate.
type controllerRegistry struct {
	credentials atomic.Pointer[Credentials]
}

func newControllerRegistry(credentials Credentials) *controllerRegistry {
	registry := &controllerRegistry{}
	registry.set(credentials)
	return registry
}

func (r *controllerRegistry) set(credentials Credentials) {
	if r != nil {
		r.credentials.Store(&credentials)
	}
}

func (r *controllerRegistry) access() (string, *http.Client) {
	credentials := r.credentials.Load()
	if credentials == nil {
		return "", nil
	}
	server, err := url.Parse(credentials.Config.Server)
	if err != nil || server.Host == "" {
		return "", nil
	}
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSClientConfig: agentTLSConfig(*credentials), TLSHandshakeTimeout: 5 * time.Second,
		IdleConnTimeout: 30 * time.Second,
	}
	return server.Host, &http.Client{Transport: transport}
}
