-- The S3 bucket every backup is copied to. The secret access key is sealed
-- with the controller's secret key like application secrets.
CREATE TABLE backup_destination (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    bucket TEXT NOT NULL,
    prefix TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    region TEXT NOT NULL,
    access_key_id TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    wrapped_key_nonce BLOB NOT NULL,
    wrapped_key BLOB NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
