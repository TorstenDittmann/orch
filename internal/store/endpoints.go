package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"

	"github.com/TorstenDittmann/orch/internal/model"
)

// ReadyServiceEndpoints returns the controller's last authenticated serving
// observations. The desired spec supplies the public ingress contract; the
// agent supplies only the address and active immutable revision.
func (s *Store) ReadyServiceEndpoints(ctx context.Context) ([]model.ServiceEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, o.node_id, o.application_name, o.service_name,
		o.address, o.declared_port, o.active_spec_digest, d.spec
		FROM allocation_observations o
		JOIN desired_allocations d ON d.id = o.id AND d.node_id = o.node_id
		JOIN nodes n ON n.id = o.node_id
		WHERE n.lifecycle != 'REMOVED' AND o.phase = 'RUNNING' AND o.address != ''
		  AND o.health_status IN ('READY', 'NOT_CONFIGURED')`)
	if err != nil {
		return nil, fmt.Errorf("read ready service endpoints: %w", err)
	}
	defer rows.Close()
	result := make([]model.ServiceEndpoint, 0)
	for rows.Next() {
		var endpoint model.ServiceEndpoint
		var port int64
		var activeDigest, encoded string
		if err := rows.Scan(&endpoint.AllocationID, &endpoint.NodeID, &endpoint.Application,
			&endpoint.Service, &endpoint.Address, &port, &activeDigest, &encoded); err != nil {
			return nil, fmt.Errorf("scan ready service endpoint: %w", err)
		}
		address, parseErr := netip.ParseAddr(endpoint.Address)
		if parseErr != nil || !address.Is4() || !address.IsPrivate() || port < 0 || port > 65535 {
			return nil, fmt.Errorf("read ready service endpoint %q: invalid address or port", endpoint.AllocationID)
		}
		var desired model.AllocationDesired
		if err := json.Unmarshal([]byte(encoded), &desired); err != nil {
			return nil, fmt.Errorf("decode ready service endpoint %q: %w", endpoint.AllocationID, err)
		}
		http, tcp := desired.HTTP, desired.TCP
		if activeDigest != "" && activeDigest != desired.SpecDigest && desired.Rollback != nil && activeDigest == desired.Rollback.SpecDigest {
			http, tcp = desired.Rollback.HTTP, desired.Rollback.TCP
		}
		endpoint.Port = uint32(port)
		if http != nil {
			endpoint.Domains = append([]string(nil), http.Domains...)
			endpoint.HTTPPublic = http.Public
			endpoint.HTTPNodeLocal = http.NodeLocal
			endpoint.HTTPS = http.HTTPS
			if http.BasicAuth != nil {
				auth := *http.BasicAuth
				endpoint.BasicAuth = &auth
			}
			if len(http.Redirects) != 0 {
				endpoint.Redirects = make(map[string]string, len(http.Redirects))
				for from, to := range http.Redirects {
					endpoint.Redirects[from] = to
				}
			}
		}
		if tcp != nil {
			endpoint.TCPPublicPort = tcp.PublicPort
		}
		result = append(result, endpoint)
	}
	if err := rows.Err(); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("iterate ready service endpoints: %w", err)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AllocationID < result[j].AllocationID })
	return result, nil
}
