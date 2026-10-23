-- Webhooks that get cluster events, set with orch notify add. The URL is a
-- secret and is stored encrypted like application secrets.
CREATE TABLE cluster_notifications (
    name TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('slack', 'discord', 'webhook')),
    events TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    wrapped_key_nonce BLOB NOT NULL,
    wrapped_key BLOB NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

-- Cluster problems that have been reported and not yet cleared, so each one
-- raises a single event however long it lasts.
CREATE TABLE cluster_alerts (
    key TEXT PRIMARY KEY,
    raised_at TEXT NOT NULL
) STRICT, WITHOUT ROWID;
