// Package tcpingress exposes byte-transparent IPv4 TCP listeners for services.
package tcpingress

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

const DefaultDrainTimeout = 30 * time.Second

type Route struct {
	Application string
	Service     string
	PublicPort  uint32
	TargetPort  uint32
	Upstreams   []string
}

type Controller struct {
	mu           sync.Mutex
	listeners    map[uint32]*listener
	drainTimeout time.Duration
	now          func() time.Time
}

type listener struct {
	route     atomic.Pointer[Route]
	listener  net.Listener
	mu        sync.Mutex
	active    map[net.Conn]struct{}
	next      atomic.Uint64
	closeOnce sync.Once
}

func New(drainTimeout time.Duration) *Controller {
	if drainTimeout <= 0 {
		drainTimeout = DefaultDrainTimeout
	}
	return &Controller{listeners: make(map[uint32]*listener), drainTimeout: drainTimeout, now: time.Now}
}

// Apply atomically retains working listeners when a replacement cannot bind.
// Existing connections drain for the configured deadline after route removal.
func (c *Controller) Apply(_ context.Context, routes []Route) []model.TCPRouteStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	desired := make(map[uint32]Route, len(routes))
	statuses := make([]model.TCPRouteStatus, 0, len(routes))
	sort.Slice(routes, func(i, j int) bool { return routes[i].PublicPort < routes[j].PublicPort })
	for _, route := range routes {
		route.Upstreams = canonicalUpstreams(route.Upstreams)
		desired[route.PublicPort] = route
		status := model.TCPRouteStatus{Application: route.Application, Service: route.Service,
			PublicPort: route.PublicPort, TargetPort: route.TargetPort, UpdatedAt: c.now().UTC()}
		if route.PublicPort == 0 || route.TargetPort == 0 {
			status.State, status.Reason, status.Message = model.TCPRouteFailed, "invalid_route", "TCP ports must be positive"
			statuses = append(statuses, status)
			continue
		}
		if current := c.listeners[route.PublicPort]; current != nil {
			copy := route
			current.route.Store(&copy)
			status.State = model.TCPRouteActive
			status.UpstreamCount = uint32(len(route.Upstreams))
			if len(route.Upstreams) == 0 {
				status.State, status.Reason, status.Message = model.TCPRoutePending, "upstreams_unavailable", "waiting for a ready service allocation"
			}
			statuses = append(statuses, status)
			continue
		}
		networkListener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(route.PublicPort))))
		if err != nil {
			status.State, status.Reason, status.Message = model.TCPRouteFailed, "listen_failed", err.Error()
			statuses = append(statuses, status)
			continue
		}
		managed := &listener{listener: networkListener, active: make(map[net.Conn]struct{})}
		copy := route
		managed.route.Store(&copy)
		c.listeners[route.PublicPort] = managed
		go managed.serve()
		status.State = model.TCPRouteActive
		status.UpstreamCount = uint32(len(route.Upstreams))
		if len(route.Upstreams) == 0 {
			status.State, status.Reason, status.Message = model.TCPRoutePending, "upstreams_unavailable", "waiting for a ready service allocation"
		}
		statuses = append(statuses, status)
	}
	for port, managed := range c.listeners {
		if _, ok := desired[port]; !ok {
			delete(c.listeners, port)
			managed.drain(c.drainTimeout)
		}
	}
	return statuses
}

func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for port, managed := range c.listeners {
		delete(c.listeners, port)
		managed.drain(0)
	}
	return nil
}

func (l *listener) serve() {
	for {
		connection, err := l.listener.Accept()
		if err != nil {
			return
		}
		go l.proxy(connection)
	}
}

func (l *listener) proxy(client net.Conn) {
	l.track(client, true)
	defer func() { l.track(client, false); _ = client.Close() }()
	route := l.route.Load()
	if route == nil || len(route.Upstreams) == 0 {
		return
	}
	index := l.next.Add(1) - 1
	upstreamAddress := net.JoinHostPort(route.Upstreams[index%uint64(len(route.Upstreams))], strconv.Itoa(int(route.TargetPort)))
	upstream, err := net.DialTimeout("tcp4", upstreamAddress, 5*time.Second)
	if err != nil {
		return
	}
	l.track(upstream, true)
	defer func() { l.track(upstream, false); _ = upstream.Close() }()
	done := make(chan struct{}, 1)
	go func() { _, _ = io.Copy(upstream, client); closeWrite(upstream); done <- struct{}{} }()
	_, _ = io.Copy(client, upstream)
	closeWrite(client)
	<-done
}

func (l *listener) track(connection net.Conn, add bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if add {
		l.active[connection] = struct{}{}
	} else {
		delete(l.active, connection)
	}
}

func (l *listener) drain(timeout time.Duration) {
	l.closeOnce.Do(func() {
		_ = l.listener.Close()
		go func() {
			if timeout > 0 {
				<-time.After(timeout)
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			for connection := range l.active {
				_ = connection.Close()
			}
		}()
	})
}

func canonicalUpstreams(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func closeWrite(connection net.Conn) {
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}

func Validate(routes []Route) error {
	seen := make(map[uint32]struct{}, len(routes))
	for _, route := range routes {
		if route.Application == "" || route.Service == "" || route.PublicPort == 0 || route.TargetPort == 0 {
			return fmt.Errorf("TCP route is incomplete")
		}
		if _, exists := seen[route.PublicPort]; exists {
			return fmt.Errorf("TCP public port %d is duplicated", route.PublicPort)
		}
		seen[route.PublicPort] = struct{}{}
	}
	return nil
}
