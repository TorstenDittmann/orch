-- A volume being moved to another node. While a row exists, the service
-- that mounts the volume is kept off every node, so its data is at rest
-- for the final backup.
CREATE TABLE volume_moves (
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE CASCADE,
    service_name TEXT NOT NULL,
    to_node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (application_name, service_name)
) STRICT;
