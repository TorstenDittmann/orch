PRAGMA legacy_alter_table = ON;

ALTER TABLE nodes RENAME TO nodes_single_node;

CREATE TABLE nodes (
    id TEXT PRIMARY KEY,
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE RESTRICT,
    name TEXT NOT NULL UNIQUE,
    public_key_hash TEXT NOT NULL UNIQUE CHECK (length(public_key_hash) = 64),
    certificate_serial TEXT NOT NULL,
    certificate_pem BLOB NOT NULL,
    certificate_not_after TEXT NOT NULL,
    created_at TEXT NOT NULL,
    last_seen_at TEXT,
    operating_system TEXT NOT NULL DEFAULT '',
    architecture TEXT NOT NULL DEFAULT '',
    runtime_version TEXT NOT NULL DEFAULT '',
    snapshotter TEXT NOT NULL DEFAULT '',
    cgroups_v2 INTEGER NOT NULL DEFAULT 0 CHECK (cgroups_v2 IN (0, 1)),
    assignment_generation INTEGER NOT NULL DEFAULT 0 CHECK (assignment_generation >= 0),
    acknowledged_generation INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged_generation >= 0 AND acknowledged_generation <= assignment_generation),
    acknowledged_digest TEXT NOT NULL DEFAULT '',
    last_boot_id TEXT NOT NULL DEFAULT '',
    last_report_sequence INTEGER NOT NULL DEFAULT 0 CHECK (last_report_sequence >= 0),
    cni_version TEXT NOT NULL DEFAULT '',
    cni_plugins_version TEXT NOT NULL DEFAULT '',
    nftables INTEGER NOT NULL DEFAULT 0 CHECK (nftables IN (0, 1)),
    dns_available INTEGER NOT NULL DEFAULT 0 CHECK (dns_available IN (0, 1)),
    caddy_version TEXT NOT NULL DEFAULT '',
    node_subnet TEXT NOT NULL DEFAULT '',
    network_generation INTEGER NOT NULL DEFAULT 0 CHECK (network_generation >= 0),
    network_state TEXT NOT NULL DEFAULT '' CHECK (network_state IN ('', 'PENDING', 'READY', 'FAILED')),
    network_reason TEXT NOT NULL DEFAULT '',
    network_message TEXT NOT NULL DEFAULT '',
    dns_state TEXT NOT NULL DEFAULT '',
    ingress_state TEXT NOT NULL DEFAULT '',
    draining_objects INTEGER NOT NULL DEFAULT 0 CHECK (draining_objects >= 0),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    lifecycle TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (lifecycle IN ('ACTIVE', 'CORDONED', 'DRAINING', 'REMOVED')),
    labels_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(labels_json) AND json_type(labels_json) = 'object'),
    ingress INTEGER NOT NULL DEFAULT 1 CHECK (ingress IN (0, 1)),
    total_cpu_millis INTEGER NOT NULL DEFAULT 0 CHECK (total_cpu_millis >= 0),
    total_memory_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_memory_bytes >= 0),
    allocatable_cpu_millis INTEGER NOT NULL DEFAULT 0 CHECK (allocatable_cpu_millis >= 0),
    allocatable_memory_bytes INTEGER NOT NULL DEFAULT 0 CHECK (allocatable_memory_bytes >= 0),
    advertise_address TEXT NOT NULL DEFAULT '',
    wireguard_public_key TEXT NOT NULL DEFAULT '',
    wireguard_listen_port INTEGER NOT NULL DEFAULT 0 CHECK (wireguard_listen_port BETWEEN 0 AND 65535),
    removed_at TEXT
) STRICT;

CREATE UNIQUE INDEX nodes_active_ordinal ON nodes(ordinal) WHERE lifecycle != 'REMOVED';
CREATE UNIQUE INDEX nodes_active_advertise_address ON nodes(advertise_address)
    WHERE lifecycle != 'REMOVED' AND advertise_address != '';
CREATE UNIQUE INDEX nodes_active_wireguard_public_key ON nodes(wireguard_public_key)
    WHERE lifecycle != 'REMOVED' AND wireguard_public_key != '';

INSERT INTO nodes (
    id, cluster_id, name, public_key_hash, certificate_serial, certificate_pem,
    certificate_not_after, created_at, last_seen_at, operating_system,
    architecture, runtime_version, snapshotter, cgroups_v2,
    assignment_generation, acknowledged_generation, acknowledged_digest,
    last_boot_id, last_report_sequence, cni_version, cni_plugins_version,
    nftables, dns_available, caddy_version, node_subnet, network_generation,
    network_state, network_reason, network_message, dns_state, ingress_state,
    draining_objects, ordinal, lifecycle, labels_json, ingress
)
SELECT id, cluster_id, name, public_key_hash, certificate_serial, certificate_pem,
    certificate_not_after, created_at, last_seen_at, operating_system,
    architecture, runtime_version, snapshotter, cgroups_v2,
    assignment_generation, acknowledged_generation, acknowledged_digest,
    last_boot_id, last_report_sequence, cni_version, cni_plugins_version,
    nftables, dns_available, caddy_version, node_subnet, network_generation,
    network_state, network_reason, network_message, dns_state, ingress_state,
    draining_objects, 0, 'ACTIVE', '{}', 1
FROM nodes_single_node;

-- SQLite rewrites child foreign-key targets when the parent is renamed while
-- foreign_keys is enabled. The child layouts do not otherwise change, so
-- point those declarations back at the replacement inventory table. Keep the
-- one-row legacy table as inert migration provenance; no runtime query uses it.
PRAGMA writable_schema = ON;
UPDATE sqlite_schema
SET sql = replace(replace(sql,
    'REFERENCES "nodes_single_node"', 'REFERENCES nodes'),
    'REFERENCES nodes_single_node', 'REFERENCES nodes')
WHERE type = 'table' AND sql LIKE '%nodes_single_node%';
PRAGMA writable_schema = OFF;
PRAGMA schema_version = 1007;
PRAGMA legacy_alter_table = OFF;

-- Every ingress node reports the same domain independently. The earlier
-- single-node key made the domain globally unique and rejected the second
-- authenticated report.
ALTER TABLE route_observations RENAME TO route_observations_single_node;
CREATE TABLE route_observations (
    domain TEXT NOT NULL,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    service_name TEXT NOT NULL,
    https INTEGER NOT NULL CHECK (https IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'ACTIVE', 'FAILED')),
    certificate_state TEXT NOT NULL,
    upstream_count INTEGER NOT NULL CHECK (upstream_count >= 0),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, domain)
) STRICT;
INSERT INTO route_observations SELECT * FROM route_observations_single_node;
DROP TABLE route_observations_single_node;
CREATE INDEX route_observations_application
    ON route_observations(application_name, service_name, domain);

CREATE TABLE node_operations (
    node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK (kind IN ('DRAIN', 'REMOVE')),
    lost INTEGER NOT NULL DEFAULT 0 CHECK (lost IN (0, 1)),
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE volume_claims (
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    volume_name TEXT NOT NULL,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (application_name, volume_name)
) STRICT;

CREATE TABLE placement_attempts (
    id TEXT PRIMARY KEY,
    logical_id TEXT NOT NULL,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    service_name TEXT NOT NULL,
    replica INTEGER NOT NULL CHECK (replica >= 1),
    state TEXT NOT NULL CHECK (state IN ('DESIRED', 'SUPERSEDED', 'REMOVED')),
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (logical_id, id)
) STRICT;

CREATE INDEX placement_attempts_node_state ON placement_attempts(node_id, state);
