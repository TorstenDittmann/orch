// Package tcpingress exposes byte-transparent IPv4 TCP listeners for services.
package tcpingress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

const DefaultDrainTimeout = 30 * time.Second

// Accept failures such as EMFILE or ECONNABORTED are retried with a capped
// exponential backoff, matching net/http.Server.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// Each listener accepts a bounded number of concurrent connections so idle or
// abusive clients cannot exhaust the agent's file descriptors, and a proxied
// connection with no bytes in either direction for idleTimeout is closed. The
// idle timeout also bounds the wait for the second direction after a half-close.
const (
	maxConnections = 4096
	idleTimeout    = 5 * time.Minute
)

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
	drained   bool
	draining  atomic.Bool
	next      atomic.Uint64
	open      atomic.Int64
	closeOnce sync.Once
	// limit and idle default to maxConnections and idleTimeout; tests lower them.
	limit int64
	idle  time.Duration
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
	var delay time.Duration
	for {
		connection, err := l.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || l.draining.Load() {
				return
			}
			if delay == 0 {
				delay = acceptRetryMin
			} else {
				delay = min(2*delay, acceptRetryMax)
			}
			time.Sleep(delay)
			continue
		}
		delay = 0
		if l.open.Add(1) > l.connectionLimit() {
			l.open.Add(-1)
			_ = connection.Close()
			continue
		}
		go func() { defer l.open.Add(-1); l.proxy(connection) }()
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
	idle := l.idle
	if idle <= 0 {
		idle = idleTimeout
	}
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())
	// The watchdog closes both sides once neither direction has moved bytes
	// for the idle timeout, which also unblocks a copy left waiting after the
	// other direction half-closed.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			if quiet := time.Since(time.Unix(0, lastActive.Load())); quiet < idle {
				timer.Reset(idle - quiet)
				continue
			}
			_ = client.Close()
			_ = upstream.Close()
			return
		}
	}()
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, activityReader{client, &lastActive})
		closeWrite(upstream)
		done <- struct{}{}
	}()
	_, _ = io.Copy(client, activityReader{upstream, &lastActive})
	closeWrite(client)
	<-done
}

func (l *listener) connectionLimit() int64 {
	if l.limit > 0 {
		return l.limit
	}
	return maxConnections
}

// activityReader records when bytes last arrived for the idle watchdog.
type activityReader struct {
	reader     io.Reader
	lastActive *atomic.Int64
}

func (r activityReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if count > 0 {
		r.lastActive.Store(time.Now().UnixNano())
	}
	return count, err
}

func (l *listener) track(connection net.Conn, add bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if add {
		// A connection tracked after the drain sweep would otherwise outlive it.
		if l.drained {
			_ = connection.Close()
			return
		}
		l.active[connection] = struct{}{}
	} else {
		delete(l.active, connection)
	}
}

func (l *listener) drain(timeout time.Duration) {
	l.closeOnce.Do(func() {
		l.draining.Store(true)
		_ = l.listener.Close()
		go func() {
			if timeout > 0 {
				<-time.After(timeout)
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			l.drained = true
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
