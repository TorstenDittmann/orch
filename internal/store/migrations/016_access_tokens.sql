CREATE TABLE access_tokens (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 application_name TEXT NOT NULL,
 read_only INTEGER NOT NULL CHECK (read_only IN (0, 1)),
 secret_hash TEXT NOT NULL,
 created_at TEXT NOT NULL,
 expires_at TEXT NOT NULL,
 last_used_at TEXT
) STRICT;
