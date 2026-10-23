package agent

import (
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// controllerRegistry gives the runtime an HTTP client for the controller's
// image endpoint that always presents the agent's current certificate. The
// client is rebuilt only when the certificate changes.
type controllerRegistry struct {
	mutex     sync.Mutex
	host      string
	client    *http.Client
	transport *http.Transport
}

func newControllerRegistry(credentials Credentials) *controllerRegistry {
	registry := &controllerRegistry{}
	registry.set(credentials)
	return registry
}

func (r *controllerRegistry) set(credentials Credentials) {
	if r == nil {
		return
	}
	host := ""
	if server, err := url.Parse(credentials.Config.Server); err == nil {
		host = server.Host
	}
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSClientConfig: agentTLSConfig(credentials), TLSHandshakeTimeout: 5 * time.Second,
		IdleConnTimeout: 60 * time.Second,
	}
	r.mutex.Lock()
	previous := r.transport
	r.host, r.client, r.transport = host, &http.Client{Transport: transport}, transport
	r.mutex.Unlock()
	if previous != nil {
		previous.CloseIdleConnections()
	}
}

func (r *controllerRegistry) access() (string, *http.Client) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.host, r.client
}
