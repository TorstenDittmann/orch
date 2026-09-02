CREATE TABLE clusters (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation >= 1),
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE RESTRICT,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    created_at TEXT NOT NULL
) STRICT;

CREATE INDEX events_cluster_sequence ON events(cluster_id, sequence);

CREATE TABLE idempotency_keys (
    key TEXT PRIMARY KEY,
    operation TEXT NOT NULL,
    response TEXT NOT NULL CHECK (json_valid(response)),
    created_at TEXT NOT NULL
) STRICT;
