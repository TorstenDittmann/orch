-- The key volume and database backups are encrypted to before they are
-- copied to the bucket. The controller creates it on first use; its private
-- half is sealed with the controller's secret key like application secrets,
-- so a restored controller can still open older copies.
CREATE TABLE volume_backup_key (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    public_key TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    wrapped_key_nonce BLOB NOT NULL,
    wrapped_key BLOB NOT NULL,
    created_at TEXT NOT NULL
) STRICT;
