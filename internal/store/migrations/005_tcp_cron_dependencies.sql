CREATE TABLE desired_cron_jobs (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    job_name TEXT NOT NULL,
    application_generation INTEGER NOT NULL CHECK (application_generation >= 1),
    revision_id TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    spec TEXT NOT NULL CHECK (json_valid(spec)),
    assignment_generation INTEGER NOT NULL CHECK (assignment_generation >= 1),
    PRIMARY KEY (node_id, application_name, job_name)
) STRICT;

CREATE TABLE tcp_route_observations (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    service_name TEXT NOT NULL,
    public_port INTEGER NOT NULL CHECK (public_port BETWEEN 1 AND 65535),
    target_port INTEGER NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'ACTIVE', 'FAILED')),
    upstream_count INTEGER NOT NULL CHECK (upstream_count >= 0),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, public_port)
) STRICT;

CREATE INDEX tcp_route_observations_application
    ON tcp_route_observations(application_name, service_name, public_port);

CREATE TABLE cron_job_observations (
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    job_name TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    next_run_at TEXT,
    active_run_id TEXT NOT NULL,
    last_run_id TEXT NOT NULL,
    last_state TEXT NOT NULL,
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, application_name, job_name)
) STRICT;

CREATE TABLE cron_run_observations (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    job_name TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    scheduled_at TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('WAITING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'TIMED_OUT', 'SKIPPED')),
    exit_code INTEGER,
    started_at TEXT,
    finished_at TEXT,
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX cron_run_observations_recent
    ON cron_run_observations(application_name, job_name, scheduled_at DESC);
