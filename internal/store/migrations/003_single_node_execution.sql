ALTER TABLE applications ADD COLUMN deleted_at TEXT;

CREATE TABLE application_execution (
    application_name TEXT PRIMARY KEY REFERENCES applications(name) ON DELETE RESTRICT,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'BLOCKED', 'STARTING', 'RUNNING', 'DEGRADED', 'FAILED', 'STOPPING', 'REMOVED')),
    observed_generation INTEGER NOT NULL CHECK (observed_generation >= 0),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

INSERT INTO application_execution
    (application_name, status, observed_generation, reason, message, updated_at)
SELECT name, 'PENDING', 0, 'agent_unavailable',
       'no agent is enrolled or connected', updated_at
FROM applications;

CREATE TABLE nodes (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    id TEXT NOT NULL UNIQUE,
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE RESTRICT,
    name TEXT NOT NULL UNIQUE,
    public_key_hash TEXT NOT NULL CHECK (length(public_key_hash) = 64),
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
    last_report_sequence INTEGER NOT NULL DEFAULT 0 CHECK (last_report_sequence >= 0)
) STRICT;

CREATE TABLE assignment_revisions (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    generation INTEGER NOT NULL CHECK (generation >= 1),
    digest TEXT NOT NULL CHECK (length(digest) = 71 AND substr(digest, 1, 7) = 'sha256:'),
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    created_at TEXT NOT NULL,
    PRIMARY KEY (node_id, generation)
) STRICT;

CREATE TRIGGER assignment_revisions_immutable_update
BEFORE UPDATE ON assignment_revisions
BEGIN
    SELECT RAISE(ABORT, 'assignment revisions are immutable');
END;

CREATE TRIGGER assignment_revisions_immutable_delete
BEFORE DELETE ON assignment_revisions
BEGIN
    SELECT RAISE(ABORT, 'assignment revisions are immutable');
END;

CREATE TABLE desired_allocations (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    service_name TEXT NOT NULL,
    replica INTEGER NOT NULL CHECK (replica >= 1),
    application_generation INTEGER NOT NULL CHECK (application_generation >= 1),
    revision_id TEXT NOT NULL REFERENCES application_revisions(id) ON DELETE RESTRICT,
    spec_digest TEXT NOT NULL CHECK (length(spec_digest) = 71 AND substr(spec_digest, 1, 7) = 'sha256:'),
    spec TEXT NOT NULL CHECK (json_valid(spec)),
    assignment_generation INTEGER NOT NULL CHECK (assignment_generation >= 1),
    UNIQUE (application_name, service_name, replica)
) STRICT;

CREATE INDEX desired_allocations_node ON desired_allocations(node_id, id);

CREATE TABLE allocation_observations (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL,
    service_name TEXT NOT NULL,
    replica INTEGER NOT NULL CHECK (replica >= 1),
    application_generation INTEGER NOT NULL CHECK (application_generation >= 1),
    spec_digest TEXT NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('PENDING', 'PULLING', 'STARTING', 'RUNNING', 'STOPPED', 'BACKOFF', 'FAILED')),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    exit_code INTEGER,
    restart_count INTEGER NOT NULL CHECK (restart_count >= 0),
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX allocation_observations_application
    ON allocation_observations(application_name, service_name, replica);
