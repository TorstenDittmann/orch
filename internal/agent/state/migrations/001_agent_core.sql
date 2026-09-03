CREATE TABLE agent_identity (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    cluster_id TEXT NOT NULL,
    node_id TEXT NOT NULL
) STRICT;

CREATE TABLE assignment (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation INTEGER NOT NULL CHECK (generation >= 1),
    digest TEXT NOT NULL CHECK (length(digest) = 71 AND substr(digest, 1, 7) = 'sha256:'),
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE desired_allocations (
    id TEXT PRIMARY KEY,
    spec_digest TEXT NOT NULL,
    spec TEXT NOT NULL CHECK (json_valid(spec))
) STRICT;

CREATE TABLE allocation_state (
    id TEXT PRIMARY KEY,
    spec_digest TEXT NOT NULL,
    phase TEXT NOT NULL,
    restart_count INTEGER NOT NULL CHECK (restart_count >= 0),
    next_attempt_at TEXT,
    exit_code INTEGER,
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
