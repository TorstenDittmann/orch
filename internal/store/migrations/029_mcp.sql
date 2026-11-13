-- The domain the remote MCP server answers on; no row means it is off.
CREATE TABLE mcp_settings (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 domain TEXT NOT NULL
) STRICT;

-- Connections an administrator approved for AI assistants on the remote MCP
-- server. refresh_hash is the SHA-256 of the current refresh token; it is
-- empty until the assistant redeems its authorization code.
CREATE TABLE mcp_grants (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 client_name TEXT NOT NULL,
 client_id TEXT NOT NULL,
 application_name TEXT NOT NULL,
 access TEXT NOT NULL CHECK (access IN ('read', 'deploy', 'destructive')),
 refresh_hash TEXT NOT NULL,
 created_at TEXT NOT NULL,
 expires_at TEXT NOT NULL,
 last_used_at TEXT
) STRICT;
