ALTER TABLE idempotency_keys RENAME TO idempotency_keys_v1;

CREATE TABLE idempotency_keys (
    key TEXT PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 128),
    operation TEXT NOT NULL CHECK (length(operation) BETWEEN 1 AND 128),
    request_hash TEXT NOT NULL CHECK (length(request_hash) = 64),
    response TEXT NOT NULL CHECK (json_valid(response)),
    created_at TEXT NOT NULL
) STRICT;

INSERT INTO idempotency_keys (key, operation, request_hash, response, created_at)
SELECT key, operation, printf('%064d', 0), response, created_at
FROM idempotency_keys_v1;

DROP TABLE idempotency_keys_v1;

CREATE TABLE applications (
    name TEXT PRIMARY KEY,
    generation INTEGER NOT NULL CHECK (generation >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE application_revisions (
    id TEXT PRIMARY KEY,
    application_name TEXT NOT NULL REFERENCES applications(name) ON DELETE RESTRICT,
    generation INTEGER NOT NULL CHECK (generation >= 1),
    digest TEXT NOT NULL CHECK (length(digest) = 71 AND substr(digest, 1, 7) = 'sha256:'),
    canonical_manifest TEXT NOT NULL CHECK (json_valid(canonical_manifest)),
    created_at TEXT NOT NULL,
    UNIQUE (application_name, generation)
) STRICT;

CREATE INDEX application_revisions_digest
    ON application_revisions(application_name, digest);

CREATE TRIGGER application_revisions_immutable_update
BEFORE UPDATE ON application_revisions
BEGIN
    SELECT RAISE(ABORT, 'application revisions are immutable');
END;

CREATE TRIGGER application_revisions_immutable_delete
BEFORE DELETE ON application_revisions
BEGIN
    SELECT RAISE(ABORT, 'application revisions are immutable');
END;
