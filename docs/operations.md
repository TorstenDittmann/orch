# Controller operations

The current implementation initializes and runs one durable controller. It
accepts authenticated application desired state, but it does not accept worker
joins, restore a full controller backup, or run containers. Application status
therefore remains `PENDING`.

## Initialize a cluster

Choose separate locations for controller state and the administrator context.
Both should live on storage controlled by the operator. Existing directories
used for private identities must not permit group or world access.

~~~console
orch cluster init \
  --name lab \
  --server https://controller.example.com:9443 \
  --data-dir /var/lib/orch \
  --context "$HOME/.config/orch/context.json"
~~~

The name is a lowercase DNS label. The server must be an HTTPS origin without
a path, query, or fragment. The data and context paths may not overlap.

The command creates this layout:

~~~text
/var/lib/orch/
├── control.db
└── pki/
    ├── ca.pem
    ├── ca-key.pem
    ├── controller.pem
    └── controller-key.pem

~/.config/orch/
├── context.json
└── identities/<cluster-id>/
    ├── ca.pem
    ├── admin.pem
    └── admin-key.pem
~~~

The context contains relative credential paths, so the context file and its
`identities` directory can be moved together. Running the same command again
validates and returns the existing cluster. It does not rewrite the database,
context, or keys. A changed name or server, or missing/mismatched identity
material, fails closed.

The first controller start also creates `/var/lib/orch/controller.lock`. The
file remains as harmless ownership metadata after a clean stop; the advisory
lock, not the file's presence, determines whether a controller is active.

## Start and stop the controller

The listen address is independent from the advertised HTTPS origin. They are
normally the same host and port; keeping them separate also permits a future
front proxy without changing durable cluster identity.

~~~console
orch controller serve \
  --data-dir /var/lib/orch \
  --listen 0.0.0.0:9443
~~~

Startup validates the database, migration history, private file permissions,
cluster CA, controller certificate/key, cluster identity, and advertised
hostname before opening the API. `controller.lock` combines an operating-system
advisory lock with in-process ownership. A second controller for the same data
directory exits without opening another listener.

SIGINT and SIGTERM trigger a bounded graceful HTTP shutdown, SQLite WAL
checkpoint, and lock release. A subsequent process can reopen the same desired
state and idempotency history.

`GET /healthz` is available over HTTPS without a client certificate for local
probes. It returns only `ok`, never cluster metadata. All ConnectRPC operator
methods require a verified administrator certificate.

## Inspect and submit desired state

Every remote CLI command uses the administrator context and mTLS:

~~~console
orch status --context "$HOME/.config/orch/context.json"

orch deploy \
  --file orch.toml \
  --context "$HOME/.config/orch/context.json"

orch events \
  --context "$HOME/.config/orch/context.json" \
  --after 0 \
  --limit 50
~~~

The controller—not the CLI—is authoritative for strict TOML parsing,
defaulting, validation, canonical JSON, and the SHA-256 digest. A changed apply
atomically commits the application generation, immutable revision, ordered
`application.desired.updated` event, and idempotency response.

`orch deploy` generates and reports a random idempotency key by default. For
automation or an explicit retry, supply a stable 16–128 character key with
`--idempotency-key`. Reusing a key with the same canonical manifest returns the
original response. Reusing it for different desired state is a conflict. A new
key applied to already-current canonical state is a no-op: it creates neither a
generation nor an event.

For this single-administrator MVP, idempotency records are retained for the
life of the cluster. A future expiration or compaction policy must preserve a
clearly versioned replay window.

The current success response is intentionally explicit:

~~~text
Status: PENDING — no agent is connected; no workload has been started.
~~~

Use `--json` for stable machine-readable status, event, and deployment output.
Event pagination uses an exclusive sequence cursor and returns the next cursor.

## Create a database backup

The controller database remains open while SQLite's online backup API creates
a consistent snapshot:

~~~console
orch cluster backup \
  --data-dir /var/lib/orch \
  --output /var/backups/orch/lab-2026-09-02.db
~~~

The output must be outside the live data directory and must not already exist.
Before publication, orch runs SQLite integrity and foreign-key checks, syncs
the staged file, and publishes it without clobbering another backup.

The database backup contains cluster state and events, but not the CA,
controller key, administrator key, or context. Preserve those files separately
with an access-controlled system backup. Automated restore over live controller
state is deliberately not implemented yet; stop and wait for a documented
restore workflow rather than copying a database over a live controller.

## Failure boundaries

- Schema migrations are forward-only, contiguous, and checksummed. A binary
  refuses changed migration history or a database created by a newer schema.
- Cluster creation and the `cluster.initialized` event commit in one database
  transaction.
- Credential files are staged and synced before no-clobber publication. If a
  normal initialization error occurs before the database transaction commits,
  orch removes only paths it created.
- A machine or process crash can still leave staged or published credentials
  beside an uninitialized database. The next initialization refuses to replace
  them. Inspect and recover or remove that state manually; never assume it is
  safe to mint a replacement administrator.
- SQLite WAL is the single controller's source of truth. There is no control
  plane high availability in this release.
- Controller state changes and idempotency responses commit in one transaction.
  A transport failure may leave the caller uncertain, so retry with the exact
  idempotency key printed or supplied for that request.
