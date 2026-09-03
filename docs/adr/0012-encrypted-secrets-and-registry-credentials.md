# ADR 0012: encrypt application secrets and registry credentials

## Status

Accepted.

## Decision

orch stores application-scoped secret values and cluster-scoped OCI registry
credentials as immutable, AES-256-GCM envelope-encrypted versions. The
controller key-encryption key is a mode `0600` file outside SQLite. Values are
write-only through the administrator API: list operations expose metadata and
version identifiers, never plaintext.

Application manifests continue to contain typed references only. A missing
referenced secret blocks that application's new intent while preserving its
last assignment. Setting or rotating a referenced secret reconciles
automatically; the version identifier participates in workload spec hashing,
so only referencing services and cron jobs roll. Registry credential versions
are assignment metadata but do not participate in workload spec hashes, so
credential rotation affects future pulls without restarting running workloads.

The controller resolves exact versions and sends a separate transient material
bundle over the existing mutually authenticated agent stream. The durable
assignment contains references only. Before acknowledging, the agent encrypts
the material again with its own mode `0600` cache key and commits it atomically
with the assignment. It can therefore restart while disconnected without
writing plaintext secrets to its SQLite database.

At the runtime boundary, secret files are projected read-only from a root-only
tmpfs. Secret environment values are read from that tmpfs by the orch runtime
initializer immediately before it replaces itself with the workload process;
they are not placed in the stored OCI specification. Registry credentials are
provided only to containerd's resolver for an exact matching image host over
HTTPS with system trust roots.

Controller backups containing encrypted records require a distinct
`--secret-key-output`. The database is unusable for secret recovery without
that file, and the two artifacts must be stored separately.

## Consequences

- Secret values remain in process memory where required and in the target
  process environment when environment projection is selected.
- Deleting a referenced application secret is rejected. Historical encrypted
  versions remain immutable for rollback authorization.
- Losing either controller or agent cache keys fails closed; orch does not
  silently replace a key for existing encrypted state.
- This slice does not add an external KMS, per-user RBAC, custom registry CAs,
  insecure registries, registry mirrors, or secret replication. Multi-node
  placement is a separate contract in ADR 0013.
