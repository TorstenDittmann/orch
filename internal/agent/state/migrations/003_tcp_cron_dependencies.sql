CREATE TABLE desired_cron_jobs (
    application_name TEXT NOT NULL,
    job_name TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    spec TEXT NOT NULL CHECK (json_valid(spec)),
    PRIMARY KEY (application_name, job_name)
) STRICT;

CREATE TABLE cron_job_state (
    application_name TEXT NOT NULL,
    job_name TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    last_scheduled_at TEXT,
    next_run_at TEXT,
    active_run_id TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (application_name, job_name)
) STRICT;

CREATE TABLE cron_run_state (
    id TEXT PRIMARY KEY,
    application_name TEXT NOT NULL,
    job_name TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    scheduled_at TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('WAITING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'TIMED_OUT', 'SKIPPED')),
    object_id TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    started_at TEXT,
    finished_at TEXT,
    reason TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX cron_run_state_recent
    ON cron_run_state(application_name, job_name, scheduled_at DESC);

CREATE TABLE tcp_route_state (
    public_port INTEGER PRIMARY KEY CHECK (public_port BETWEEN 1 AND 65535),
    application_name TEXT NOT NULL,
    service_name TEXT NOT NULL,
    target_port INTEGER NOT NULL CHECK (target_port BETWEEN 1 AND 65535),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'ACTIVE', 'FAILED')),
    upstream_count INTEGER NOT NULL CHECK (upstream_count >= 0),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
