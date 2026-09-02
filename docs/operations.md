# Controller operations

The current implementation initializes durable state for one controller. It
does not start a daemon, accept worker joins, restore a backup, or run
containers.

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
