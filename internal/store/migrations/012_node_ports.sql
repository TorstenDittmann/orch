CREATE TABLE node_port_observations (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    service_name TEXT NOT NULL,
    allocation_id TEXT NOT NULL,
    protocol TEXT NOT NULL CHECK (protocol IN ('tcp', 'udp')),
    node_port INTEGER NOT NULL CHECK (node_port BETWEEN 1 AND 65535),
    target_port INTEGER NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'ACTIVE', 'FAILED')),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, protocol, node_port, allocation_id)
) STRICT;

CREATE INDEX node_port_observations_application
    ON node_port_observations(application_name, service_name, protocol, node_port);
