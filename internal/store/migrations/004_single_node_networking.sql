ALTER TABLE clusters ADD COLUMN workload_cidr TEXT NOT NULL DEFAULT '10.88.0.0/16';
ALTER TABLE clusters ADD COLUMN network_generation INTEGER NOT NULL DEFAULT 1 CHECK (network_generation >= 1);
ALTER TABLE clusters ADD COLUMN network_frozen_at TEXT;
ALTER TABLE clusters ADD COLUMN network_updated_at TEXT NOT NULL DEFAULT '';

UPDATE clusters SET network_updated_at = created_at WHERE network_updated_at = '';

ALTER TABLE nodes ADD COLUMN cni_version TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN cni_plugins_version TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN nftables INTEGER NOT NULL DEFAULT 0 CHECK (nftables IN (0, 1));
ALTER TABLE nodes ADD COLUMN dns_available INTEGER NOT NULL DEFAULT 0 CHECK (dns_available IN (0, 1));
ALTER TABLE nodes ADD COLUMN caddy_version TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN node_subnet TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN network_generation INTEGER NOT NULL DEFAULT 0 CHECK (network_generation >= 0);
ALTER TABLE nodes ADD COLUMN network_state TEXT NOT NULL DEFAULT '' CHECK (network_state IN ('', 'PENDING', 'READY', 'FAILED'));
ALTER TABLE nodes ADD COLUMN network_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN network_message TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN dns_state TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN ingress_state TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN draining_objects INTEGER NOT NULL DEFAULT 0 CHECK (draining_objects >= 0);

ALTER TABLE application_execution RENAME TO application_execution_v3;

CREATE TABLE application_execution (
    application_name TEXT PRIMARY KEY REFERENCES applications(name) ON DELETE RESTRICT,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'BLOCKED', 'STARTING', 'RUNNING', 'DEGRADED', 'FAILED', 'STOPPING', 'REMOVED', 'HEALTHY', 'ROLLED_BACK')),
    observed_generation INTEGER NOT NULL CHECK (observed_generation >= 0),
    active_generation INTEGER NOT NULL DEFAULT 0 CHECK (active_generation >= 0),
    active_revision_id TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO application_execution
    (application_name, status, observed_generation, active_generation, active_revision_id, reason, message, updated_at)
SELECT application_name, status, observed_generation,
       CASE WHEN status IN ('RUNNING', 'DEGRADED') THEN observed_generation ELSE 0 END,
       CASE WHEN status IN ('RUNNING', 'DEGRADED') THEN COALESCE((
           SELECT id FROM application_revisions
           WHERE application_name = application_execution_v3.application_name
             AND generation = application_execution_v3.observed_generation
       ), '') ELSE '' END,
       reason, message, updated_at
FROM application_execution_v3;

DROP TABLE application_execution_v3;

ALTER TABLE allocation_observations ADD COLUMN health_status TEXT NOT NULL DEFAULT 'NOT_CONFIGURED'
    CHECK (health_status IN ('NOT_CONFIGURED', 'CHECKING', 'READY', 'UNHEALTHY'));
ALTER TABLE allocation_observations ADD COLUMN address TEXT NOT NULL DEFAULT '';
ALTER TABLE allocation_observations ADD COLUMN active_spec_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE allocation_observations ADD COLUMN active_generation INTEGER NOT NULL DEFAULT 0 CHECK (active_generation >= 0);
ALTER TABLE allocation_observations ADD COLUMN active_revision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE allocation_observations ADD COLUMN ready_at TEXT;
ALTER TABLE allocation_observations ADD COLUMN declared_port INTEGER NOT NULL DEFAULT 0
    CHECK (declared_port >= 0 AND declared_port <= 65535);

UPDATE allocation_observations
SET active_spec_digest = spec_digest,
    active_generation = application_generation,
    active_revision_id = COALESCE((
        SELECT revision_id FROM desired_allocations
        WHERE desired_allocations.id = allocation_observations.id
    ), '')
WHERE phase = 'RUNNING';

CREATE TABLE route_observations (
    domain TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    service_name TEXT NOT NULL,
    https INTEGER NOT NULL CHECK (https IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'ACTIVE', 'FAILED')),
    certificate_state TEXT NOT NULL,
    upstream_count INTEGER NOT NULL CHECK (upstream_count >= 0),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX route_observations_application
    ON route_observations(application_name, service_name, domain);
