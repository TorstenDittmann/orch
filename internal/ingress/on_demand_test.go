package ingress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOnDemandPolicyLimitsNewNamesPerWildcard(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	stateDir := t.TempDir()
	policy := newOnDemandPolicy(stateDir, func() time.Time { return now })
	policy.setWildcards([]Route{
		{Domain: "*.example.com", HTTPS: true},
		{Domain: "*.plain.example.com"},
		{Domain: "api.example.com", HTTPS: true},
	})
	for _, name := range []string{"api.example.com.evil", "example.com", "a.b.example.com", "x.plain.example.com", "x.other.com", "", "-x.example.com"} {
		if policy.allow(name) {
			t.Fatalf("allow(%q) = true, want false", name)
		}
	}
	for index := range OnDemandNewNamesPerDay {
		if !policy.allow(fmt.Sprintf("tenant%d.example.com", index)) {
			t.Fatalf("new name %d was refused below the limit", index)
		}
	}
	if policy.allow("one-too-many.example.com") {
		t.Fatal("a new name over the daily limit was allowed")
	}
	if !policy.allow("TENANT3.example.com.") {
		t.Fatal("a name allowed before was refused once the limit was reached")
	}

	// The names survive a restart, and the limit frees up a day later.
	now = now.Add(onDemandWindow)
	restarted := newOnDemandPolicy(stateDir, func() time.Time { return now })
	restarted.setWildcards([]Route{{Domain: "*.example.com", HTTPS: true}})
	if !restarted.allow("tenant0.example.com") || !restarted.allow("one-too-many.example.com") {
		t.Fatal("restart forgot allowed names or kept counting yesterday's")
	}
	if info, err := os.Stat(filepath.Join(stateDir, onDemandNamesFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("names file = %v, %v; want a private file", info, err)
	}
	// A wildcard that is no longer routed gets nothing.
	restarted.setWildcards(nil)
	if restarted.allow("tenant0.example.com") {
		t.Fatal("a name was allowed after its wildcard route went away")
	}
}

func TestOnDemandServerAnswersOverUnixSocket(t *testing.T) {
	directory, err := os.MkdirTemp("", "od")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	policy := newOnDemandPolicy(t.TempDir(), time.Now)
	policy.setWildcards([]Route{{Domain: "*.example.com", HTTPS: true}})
	socket := filepath.Join(directory, onDemandSocketName)
	server, err := startOnDemandServer(socket, policy, processIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket = %v, %v; want mode 0600", info, err)
	}
	client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	for domain, want := range map[string]int{"shop.example.com": http.StatusOK, "shop.other.com": http.StatusForbidden} {
		response, err := client.Get("http://127.0.0.1" + onDemandPermissionPath + "?domain=" + domain)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s = %d, want %d", domain, response.StatusCode, want)
		}
	}
}
