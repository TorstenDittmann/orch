package ingress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type recoveryTransport func(*http.Request) (*http.Response, error)

func (f recoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInterruptedCaddyLoadRecoversDurableFingerprint(t *testing.T) {
	for _, timeoutAt := range []string{"load", "ping"} {
		t.Run(timeoutAt, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			old := []byte(`{"apps":{"http":{"servers":{}}}}`)
			candidate := []byte(`{"apps":{"http":{"servers":{"new":{}}}}}`)
			acceptedPath := filepath.Join(dir, "accepted-config.json")
			candidatePath := filepath.Join(dir, "candidate-config.json")
			if err := publishAgentFile(acceptedPath, old, 0600); err != nil {
				t.Fatal(err)
			}
			if err := publishAgentFile(candidatePath, candidate, 0600); err != nil {
				t.Fatal(err)
			}
			live := old
			interrupted := false
			caddy := &Caddy{options: CaddyOptions{StateDir: dir, DataDir: dir}, identity: processIdentity{uid: os.Getuid(), gid: os.Getgid()}}
			caddy.client = &http.Client{Transport: recoveryTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					var err error
					live, err = io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if timeoutAt == "load" && !interrupted {
						interrupted = true
						return nil, context.DeadlineExceeded // Accepted, but response lost.
					}
				} else if timeoutAt == "ping" && !interrupted {
					interrupted = true
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(live))}, nil
			})}
			if _, err := caddy.loadCandidate(context.Background(), candidate); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("interrupted load = %v", err)
			}
			if !bytes.Equal(live, candidate) {
				t.Fatal("test did not reproduce an accepted config with a lost response")
			}
			// A later pass must authenticate the live candidate and finish its
			// durable commit without restarting Caddy or trusting unknown config.
			if err := caddy.validateLiveConfiguration(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{acceptedPath, filepath.Join(dir, "config.json")} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, candidate) {
					t.Fatalf("recovered file %s = %s, %v", path, got, err)
				}
			}
			if _, err := os.Stat(candidatePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("candidate was not promoted: %v", err)
			}
			if err := caddy.validateLiveConfiguration(context.Background()); err != nil {
				t.Fatalf("next pass still mismatches: %v", err)
			}
			live = []byte(`{"untrusted":true}`)
			if err := caddy.validateLiveConfiguration(context.Background()); err == nil {
				t.Fatal("adopted an unknown live configuration")
			}
		})
	}
}
