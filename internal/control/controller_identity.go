package control

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"log/slog"
	"sync"
	"time"

	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/model"
)

// controllerRenewBefore is how long before expiry the controller reissues its
// own server certificate: ahead of the 30-day expiry alert, so the alert only
// fires when renewal keeps failing. Agents and clients check the leaf only
// against the CA, so they pick up a new one on their next connection.
const controllerRenewBefore = 45 * 24 * time.Hour

// controllerIdentityCheckInterval is a variable so tests can shorten it.
var controllerIdentityCheckInterval = time.Hour

// controllerIdentity holds the TLS certificate the controller serves and swaps
// it when the controller renews itself, without a restart.
type controllerIdentity struct {
	mu          sync.RWMutex
	certificate *tls.Certificate
	notAfter    time.Time
	renew       func(now time.Time) (clusterctl.ControllerCredentials, error)
	now         func() time.Time
}

func newControllerIdentity(credentials clusterctl.ControllerCredentials, now func() time.Time,
	renew func(time.Time) (clusterctl.ControllerCredentials, error)) *controllerIdentity {
	identity := &controllerIdentity{renew: renew, now: now}
	identity.set(credentials)
	return identity
}

func renewControllerLeafIn(dataDir string, cluster model.Cluster) func(time.Time) (clusterctl.ControllerCredentials, error) {
	return func(now time.Time) (clusterctl.ControllerCredentials, error) {
		return clusterctl.RenewControllerLeaf(dataDir, cluster, now, nil)
	}
}

// set serves the leaf followed by the CA, so a joining node can pin the CA by
// fingerprint from a join token before it has the CA certificate.
func (c *controllerIdentity) set(credentials clusterctl.ControllerCredentials) {
	served := credentials.Certificate
	if block, _ := pem.Decode(credentials.CACertificatePEM); block != nil {
		served.Certificate = append(append([][]byte(nil), served.Certificate...), block.Bytes)
	}
	var notAfter time.Time
	if served.Leaf != nil {
		notAfter = served.Leaf.NotAfter
	}
	c.mu.Lock()
	c.certificate, c.notAfter = &served, notAfter
	c.mu.Unlock()
}

func (c *controllerIdentity) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.certificate, nil
}

func (c *controllerIdentity) NotAfter() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.notAfter
}

// renewIfDue reissues the certificate once it is within controllerRenewBefore
// of expiry. A failure keeps the current certificate and is retried on the
// next check; the cluster alert for an expiring certificate still fires.
func (c *controllerIdentity) renewIfDue() bool {
	now := c.now()
	notAfter := c.NotAfter()
	if notAfter.IsZero() || notAfter.Sub(now) > controllerRenewBefore {
		return false
	}
	credentials, err := c.renew(now)
	if err != nil {
		slog.Error("renew controller certificate", "not_after", notAfter, "error", err)
		return false
	}
	c.set(credentials)
	slog.Info("renewed controller certificate", "previous_not_after", notAfter, "not_after", c.NotAfter())
	return true
}

func (c *controllerIdentity) run(ctx context.Context) {
	c.renewIfDue()
	ticker := time.NewTicker(controllerIdentityCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.renewIfDue()
		}
	}
}
