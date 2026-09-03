ALTER TABLE allocation_state ADD COLUMN health_status TEXT NOT NULL DEFAULT 'NOT_CONFIGURED'
    CHECK (health_status IN ('NOT_CONFIGURED', 'CHECKING', 'READY', 'UNHEALTHY'));
ALTER TABLE allocation_state ADD COLUMN address TEXT NOT NULL DEFAULT '';
ALTER TABLE allocation_state ADD COLUMN active_spec_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE allocation_state ADD COLUMN active_generation INTEGER NOT NULL DEFAULT 0 CHECK (active_generation >= 0);
ALTER TABLE allocation_state ADD COLUMN active_revision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE allocation_state ADD COLUMN ready_at TEXT;
ALTER TABLE allocation_state ADD COLUMN consecutive_successes INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_successes >= 0);
ALTER TABLE allocation_state ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0);
ALTER TABLE allocation_state ADD COLUMN last_probe_at TEXT;

CREATE TABLE network_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation INTEGER NOT NULL CHECK (generation >= 1),
    cluster_cidr TEXT NOT NULL,
    node_subnet TEXT NOT NULL,
    gateway TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'READY', 'FAILED')),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    dns_state TEXT NOT NULL,
    ingress_state TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE network_attachments (
    allocation_id TEXT NOT NULL,
    object_id TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    network_generation INTEGER NOT NULL CHECK (network_generation >= 1),
    address TEXT NOT NULL,
    netns TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (allocation_id, object_id)
) STRICT;

CREATE UNIQUE INDEX network_attachments_object ON network_attachments(object_id);

CREATE TABLE route_state (
    domain TEXT PRIMARY KEY,
    application_name TEXT NOT NULL,
    service_name TEXT NOT NULL,
    https INTEGER NOT NULL CHECK (https IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'ACTIVE', 'FAILED')),
    certificate_state TEXT NOT NULL,
    upstream_count INTEGER NOT NULL CHECK (upstream_count >= 0),
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE serving_state (
    allocation_id TEXT PRIMARY KEY,
    application_name TEXT NOT NULL,
    service_name TEXT NOT NULL,
    address TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE candidate_serving_state (
    allocation_id TEXT PRIMARY KEY,
    application_name TEXT NOT NULL,
    service_name TEXT NOT NULL,
    address TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE drain_state (
    object_id TEXT PRIMARY KEY,
    allocation_id TEXT NOT NULL,
    spec_digest TEXT NOT NULL,
    drain_until TEXT NOT NULL
) STRICT;
