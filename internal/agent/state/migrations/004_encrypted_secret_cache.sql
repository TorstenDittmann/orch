CREATE TABLE secret_cache_key_metadata (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  fingerprint TEXT NOT NULL
) STRICT;

CREATE TABLE secret_materials (
  kind TEXT NOT NULL,
  application_name TEXT NOT NULL,
  secret_name TEXT NOT NULL,
  registry_host TEXT NOT NULL,
  username TEXT NOT NULL,
  version_id TEXT NOT NULL,
  algorithm TEXT NOT NULL,
  nonce BLOB NOT NULL,
  ciphertext BLOB NOT NULL,
  wrapped_key_nonce BLOB NOT NULL,
  wrapped_key BLOB NOT NULL,
  value_hmac TEXT NOT NULL,
  assignment_generation INTEGER NOT NULL,
  PRIMARY KEY (kind, application_name, secret_name, registry_host, version_id)
) STRICT;

CREATE TRIGGER secret_materials_immutable_update
BEFORE UPDATE ON secret_materials BEGIN
  SELECT RAISE(ABORT, 'secret material rows are immutable');
END;
