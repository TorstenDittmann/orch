package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

const (
	ProbeInterval     = 2 * time.Second
	ProbeTimeout      = time.Second
	ReadySuccesses    = 2
	UnhealthyFailures = 3
	maxResponseBytes  = 4 << 10
)

var serviceHostPattern = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?\.[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?\.svc\.internal$`)

type Target struct {
	Address netip.Addr
	Port    uint16
	Path    string
	Host    string
}

type Prober interface {
	Probe(context.Context, Target) error
}

type NetworkProber struct {
	dialer interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	}
}

func NewNetworkProber() *NetworkProber {
	return &NetworkProber{dialer: &net.Dialer{Timeout: ProbeTimeout}}
}

func (p *NetworkProber) Probe(ctx context.Context, target Target) error {
	if !target.Address.IsValid() || !target.Address.Is4() || !target.Address.IsPrivate() || target.Port == 0 {
		return fmt.Errorf("health target requires a private IPv4 address and port")
	}
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	address := net.JoinHostPort(target.Address.String(), fmt.Sprintf("%d", target.Port))
	if target.Path == "" {
		connection, err := p.dialer.DialContext(probeCtx, "tcp", address)
		if err != nil {
			return fmt.Errorf("TCP readiness failed: %w", err)
		}
		return connection.Close()
	}
	if !strings.HasPrefix(target.Path, "/") || strings.ContainsAny(target.Path, "\r\n") || !serviceHostPattern.MatchString(target.Host) {
		return fmt.Errorf("HTTP health target has an invalid path or host")
	}
	requestURL := (&url.URL{Scheme: "http", Host: address, Path: target.Path}).String()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("build HTTP readiness request: %w", err)
	}
	request.Host = target.Host
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           p.dialer.DialContext,
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: ProbeTimeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("HTTP readiness failed: %w", err)
	}
	defer response.Body.Close()
	if _, err := io.CopyN(io.Discard, response.Body, maxResponseBytes); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read bounded HTTP readiness response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 399 {
		return fmt.Errorf("HTTP readiness returned status %d", response.StatusCode)
	}
	return nil
}

type Tracker struct {
	Status    string
	Successes uint32
	Failures  uint32
	ReadyAt   *time.Time
}

func (t Tracker) Observe(success bool, now time.Time) (Tracker, bool) {
	previous := t.Status
	if previous == "" || previous == model.HealthNotConfigured {
		t.Status = model.HealthChecking
	}
	if success {
		t.Successes++
		t.Failures = 0
		if t.Successes >= ReadySuccesses {
			t.Status = model.HealthReady
			if t.ReadyAt == nil {
				stamp := now.UTC()
				t.ReadyAt = &stamp
			}
		}
	} else {
		t.Failures++
		t.Successes = 0
		if t.Failures >= UnhealthyFailures {
			t.Status = model.HealthUnhealthy
			t.ReadyAt = nil
		}
	}
	return t, previous != t.Status
}

type Fake struct {
	Err     error
	Targets []Target
}

func (f *Fake) Probe(_ context.Context, target Target) error {
	f.Targets = append(f.Targets, target)
	return f.Err
}
