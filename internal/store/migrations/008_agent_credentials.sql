CREATE TABLE join_tokens (
    id TEXT PRIMARY KEY,
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE RESTRICT,
    node_name TEXT NOT NULL,
    secret_hash TEXT NOT NULL CHECK (length(secret_hash) = 64),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT,
    consumed_at TEXT,
    consumed_request_hash TEXT NOT NULL DEFAULT '',
    consumed_idempotency_key TEXT NOT NULL DEFAULT '',
    CHECK (revoked_at IS NULL OR consumed_at IS NULL)
) STRICT;

CREATE INDEX join_tokens_expiry ON join_tokens(expires_at);
CREATE INDEX join_tokens_node ON join_tokens(node_name, expires_at);

ALTER TABLE nodes ADD COLUMN pending_certificate_serial TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN pending_certificate_pem BLOB NOT NULL DEFAULT X'';
ALTER TABLE nodes ADD COLUMN pending_certificate_not_after TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN pending_certificate_request_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN pending_certificate_issued_at TEXT NOT NULL DEFAULT '';
