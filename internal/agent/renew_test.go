package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
)

func TestCertificateRenewalScheduleAndBackoff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	nodeID := "node-0123456789abcdef"
	jitter := renewalJitter(nodeID)
	if jitter < 0 || jitter >= renewalJitterRange || jitter != renewalJitter(nodeID) {
		t.Fatalf("renewal jitter = %s", jitter)
	}
	runner := &sessionRunner{credentials: Credentials{Config: Config{NodeID: nodeID}, Certificate: tls.Certificate{Leaf: &x509.Certificate{
		NotAfter: now.Add(renewalWindow).Add(-jitter).Add(time.Hour),
	}}}, now: func() time.Time { return now }}
	if delay := runner.certificateRenewalDelay(); delay != time.Hour {
		t.Fatalf("renewal delay = %s, want 1h", delay)
	}
	runner.noteRenewalFailure()
	if runner.renewalBackoff != 5*time.Minute || !runner.renewalRetryAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("first backoff = %s at %s", runner.renewalBackoff, runner.renewalRetryAt)
	}
	completed, err := runner.maybeRenewCertificate(context.Background())
	if err != nil || completed {
		t.Fatalf("deferred renewal = %t, %v", completed, err)
	}
	if runner.renewalBackoff != 5*time.Minute || !runner.renewalRetryAt.Equal(now.Add(5*time.Minute)) {
		t.Fatal("deferred renewal mutated retry state")
	}
	for range 10 {
		runner.noteRenewalFailure()
	}
	if runner.renewalBackoff != 6*time.Hour {
		t.Fatalf("maximum backoff = %s", runner.renewalBackoff)
	}
}

func TestCertificateExpiryBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	runner := &sessionRunner{credentials: Credentials{Certificate: tls.Certificate{Leaf: &x509.Certificate{NotAfter: now}}}, now: func() time.Time { return now }}
	if !runner.certificateExpired() {
		t.Fatal("certificate was not expired at its declared expiry instant")
	}
}
