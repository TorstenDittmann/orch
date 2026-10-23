package control

import (
	"context"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
)

const (
	clusterAlertEvery = time.Minute
	// nodeLostAfter is how long a node may go without a heartbeat before it
	// counts as lost; agent restarts and upgrades take far less.
	nodeLostAfter = 3 * time.Minute
	// A node's disk is low from diskLowPercent full until it drops below
	// diskClearPercent, so a disk hovering at the line raises one alert.
	diskLowPercent   = 90
	diskClearPercent = 85
	// certificateExpiryWarning matches orch status and orch host doctor.
	// Agents renew 90 days ahead, so an agent certificate this close to
	// expiry means renewal is failing.
	certificateExpiryWarning = 30 * 24 * time.Hour
	// controllerBackupStaleAfter matches orch status.
	controllerBackupStaleAfter = 48 * time.Hour
	// administratorSeenWithin limits expiry alerts to administrator
	// certificates that were used recently.
	administratorSeenWithin = 7 * 24 * time.Hour
)

// clusterAlerts watches the cluster itself and appends an event when a
// problem starts: a lost node, a full disk, a certificate close to expiry or
// a stale controller backup. Each problem is recorded so it is reported once,
// and forgotten when it clears so it is reported again if it comes back.
type clusterAlerts struct {
	service            *service
	controllerNotAfter time.Time
	caNotAfter         time.Time
	startedAt          time.Time
}

func newClusterAlerts(s *service, controllerNotAfter time.Time) *clusterAlerts {
	caNotAfter, _ := security.CertificateNotAfter(s.caPEM)
	return &clusterAlerts{service: s, controllerNotAfter: controllerNotAfter, caNotAfter: caNotAfter, startedAt: s.now()}
}

func (a *clusterAlerts) run(ctx context.Context) {
	ticker := time.NewTicker(clusterAlertEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := a.check(ctx); err != nil && ctx.Err() == nil {
			log.Printf("controller: cluster alerts: %v", err)
		}
	}
}

// alert is one problem that is present now.
type alert struct {
	kind    string
	payload model.ClusterAlert
}

// check raises every present problem and clears those that went away.
func (a *clusterAlerts) check(ctx context.Context) error {
	s := a.service
	now := s.now()
	raised, err := s.state.ClusterAlerts(ctx)
	if err != nil {
		return err
	}
	wasRaised := make(map[string]bool, len(raised))
	for _, key := range raised {
		wasRaised[key] = true
	}
	active := map[string]alert{}
	// keep holds raised problems that could not be checked this time, such
	// as the disk of a node whose usage report is stale.
	keep := map[string]bool{}

	nodes, err := s.state.Nodes(ctx)
	if err != nil {
		return err
	}
	usage, err := s.state.LatestNodeUsage(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if node.Lifecycle == model.NodeLifecycleRemoved {
			continue
		}
		if a.lost(node, now) {
			active["node.lost/"+node.ID] = alert{config.NotifyNodeLost, model.ClusterAlert{Node: node.Name,
				Message: fmt.Sprintf("node %s is unreachable; last heartbeat %s", node.Name, node.LastSeenAt.UTC().Format(time.RFC3339))}}
		} else if !a.connected(node) {
			// A lost node is cleared only once it is back, not while the
			// controller's startup grace hides it.
			keep["node.lost/"+node.ID] = true
		}
		diskKey := "node.disk_low/" + node.ID
		if sample, ok := usage[node.ID]; ok && now.Sub(sample.ObservedAt) <= nodeUsageFresh && sample.DiskTotalBytes > 0 {
			percent := float64(sample.DiskUsedBytes) * 100 / float64(sample.DiskTotalBytes)
			if percent >= diskLowPercent || (wasRaised[diskKey] && percent >= diskClearPercent) {
				active[diskKey] = alert{config.NotifyNodeDiskLow, model.ClusterAlert{Node: node.Name,
					Message: fmt.Sprintf("node %s disk is %.0f%% full (%s of %s used)", node.Name, percent, gigabytes(sample.DiskUsedBytes), gigabytes(sample.DiskTotalBytes))}}
			}
		} else {
			keep[diskKey] = true
		}
		if !node.CertificateNotAfter.IsZero() {
			a.certificate(active, now, "agent certificate of node "+node.Name, node.CertificateNotAfter,
				"agents renew it 90 days ahead, so renewal is failing; check the agent's logs on "+node.Name)
		}
	}

	renew := "run orch cluster certificates renew on the controller host"
	a.certificate(active, now, "controller certificate", a.controllerNotAfter, renew)
	a.certificate(active, now, "cluster CA certificate", a.caNotAfter, renew)
	if s.administrators != nil {
		for _, notAfter := range s.administrators.recent(now) {
			a.certificate(active, now, "administrator certificate", notAfter, renew+" with --admin-dir NEW_DIR")
		}
	}

	if backups := s.backups; backups != nil {
		status := backups.status()
		since := status.Latest.CreatedAt
		if since.IsZero() {
			since = a.startedAt
		}
		if now.Sub(since) >= controllerBackupStaleAfter {
			message := "no controller backup has been written since the controller started"
			if !status.Latest.CreatedAt.IsZero() {
				message = "the newest controller backup is from " + status.Latest.CreatedAt.UTC().Format(time.RFC3339) + ", more than 2 days ago"
			}
			if status.Failure != "" {
				message += "; the last attempt failed: " + status.Failure
			}
			active["backup.stale"] = alert{config.NotifyBackupStale, model.ClusterAlert{Message: message}}
		}
	}

	keys := make([]string, 0, len(active))
	for key := range active {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if wasRaised[key] {
			continue
		}
		if _, err := s.state.RaiseClusterAlert(ctx, key, active[key].kind, active[key].payload, now); err != nil {
			return err
		}
	}
	for _, key := range raised {
		if _, ok := active[key]; !ok && !keep[key] {
			if err := s.state.ClearClusterAlert(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// lost reports whether a node has had no heartbeat for nodeLostAfter. Time
// before the controller started does not count, so restarting a controller
// that was down for a while does not report every node as lost.
func (a *clusterAlerts) lost(node model.Node, now time.Time) bool {
	if node.LastSeenAt == nil || a.connected(node) {
		return false
	}
	since := *node.LastSeenAt
	if since.Before(a.startedAt) {
		since = a.startedAt
	}
	return now.Sub(since) >= nodeLostAfter
}

func (a *clusterAlerts) connected(node model.Node) bool {
	return a.service.sessions != nil && a.service.sessions.Connected(node.ID)
}

// certificate adds an alert when notAfter is within the warning window. The
// key includes the expiry, so a renewed certificate starts over.
func (a *clusterAlerts) certificate(active map[string]alert, now time.Time, subject string, notAfter time.Time, remedy string) {
	if notAfter.IsZero() || notAfter.Sub(now) >= certificateExpiryWarning {
		return
	}
	verb := "expires"
	if !now.Before(notAfter) {
		verb = "expired"
	}
	key := fmt.Sprintf("certificate.expiring/%s/%d", strings.ReplaceAll(subject, " ", "-"), notAfter.Unix())
	active[key] = alert{config.NotifyCertificateExpiring, model.ClusterAlert{Subject: subject, NotAfter: notAfter.UTC(),
		Message: fmt.Sprintf("%s %s %s; %s", subject, verb, notAfter.UTC().Format(time.RFC3339), remedy)}}
}

func gigabytes(value uint64) string {
	return fmt.Sprintf("%.1f GB", float64(value)/1e9)
}

// administratorCertificates remembers the long-lived administrator
// certificates that call the API, which the controller does not otherwise
// keep, so their expiry can be reported.
type administratorCertificates struct {
	mu   sync.Mutex
	seen map[string]seenCertificate
}

type seenCertificate struct {
	notAfter time.Time
	lastUsed time.Time
}

// observe wraps an authenticated administrator handler and records the
// certificate of each administrator request. Certificates from access
// tokens live for minutes and renew themselves, so they are left out.
func (c *administratorCertificates) observe(now func() time.Time, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if principal, ok := request.Context().Value(principalKey{}).(authenticatedPrincipal); ok &&
			principal.err == nil && principal.certificate != nil && principal.application == "" && !principal.readOnly {
			c.record(principal.certificate, now())
		}
		next.ServeHTTP(response, request)
	})
}

func (c *administratorCertificates) record(certificate *x509.Certificate, now time.Time) {
	if certificate.NotAfter.Sub(certificate.NotBefore) <= certificateExpiryWarning {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]seenCertificate{}
	}
	c.seen[certificate.SerialNumber.String()] = seenCertificate{notAfter: certificate.NotAfter, lastUsed: now}
}

// recent returns the expiry of each certificate used within
// administratorSeenWithin, and forgets the others.
func (c *administratorCertificates) recent(now time.Time) []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []time.Time
	for serial, item := range c.seen {
		if now.Sub(item.lastUsed) > administratorSeenWithin {
			delete(c.seen, serial)
			continue
		}
		result = append(result, item.notAfter)
	}
	return result
}
