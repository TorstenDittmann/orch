CREATE TABLE secret_key_metadata (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    fingerprint TEXT NOT NULL CHECK (length(fingerprint) = 64),
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE application_secrets (
    application_name TEXT NOT NULL,
    name TEXT NOT NULL,
    current_version_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
	deleted_at TEXT,
    PRIMARY KEY (application_name, name)
) STRICT;

CREATE TABLE application_secret_versions (
    id TEXT PRIMARY KEY,
    application_name TEXT NOT NULL,
    name TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    wrapped_key_nonce BLOB NOT NULL,
    wrapped_key BLOB NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY (application_name, name) REFERENCES application_secrets(application_name, name) ON DELETE RESTRICT,
    UNIQUE (application_name, name, id)
) STRICT;

CREATE INDEX application_secret_versions_scope
    ON application_secret_versions(application_name, name, created_at DESC);

CREATE TRIGGER application_secret_versions_immutable_update
BEFORE UPDATE ON application_secret_versions
BEGIN
    SELECT RAISE(ABORT, 'application secret versions are immutable');
END;

CREATE TRIGGER application_secret_versions_immutable_delete
BEFORE DELETE ON application_secret_versions
BEGIN
    SELECT RAISE(ABORT, 'application secret versions are immutable');
END;

CREATE TABLE registry_credentials (
    host TEXT PRIMARY KEY,
    username TEXT NOT NULL,
    current_version_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	deleted_at TEXT
) STRICT;

CREATE TABLE registry_credential_versions (
    id TEXT PRIMARY KEY,
    host TEXT NOT NULL REFERENCES registry_credentials(host) ON DELETE RESTRICT,
    username TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    wrapped_key_nonce BLOB NOT NULL,
    wrapped_key BLOB NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (host, id)
) STRICT;

CREATE INDEX registry_credential_versions_host
    ON registry_credential_versions(host, created_at DESC);

CREATE TRIGGER registry_credential_versions_immutable_update
BEFORE UPDATE ON registry_credential_versions
BEGIN
    SELECT RAISE(ABORT, 'registry credential versions are immutable');
END;

CREATE TRIGGER registry_credential_versions_immutable_delete
BEFORE DELETE ON registry_credential_versions
BEGIN
    SELECT RAISE(ABORT, 'registry credential versions are immutable');
END;
