CREATE TABLE node_usage_samples (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    observed_at INTEGER NOT NULL,
    cpu_used_millis INTEGER NOT NULL CHECK (cpu_used_millis >= 0),
    memory_used_bytes INTEGER NOT NULL CHECK (memory_used_bytes >= 0),
    disk_used_bytes INTEGER NOT NULL CHECK (disk_used_bytes >= 0),
    disk_total_bytes INTEGER NOT NULL CHECK (disk_total_bytes >= 0),
    PRIMARY KEY (node_id, observed_at)
) STRICT, WITHOUT ROWID;
