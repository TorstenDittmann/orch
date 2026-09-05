# ADR 020: Controller recovery from a verified snapshot

Status: accepted

## Context

The controller is a single SQLite authority. Online database backup already
preserves its durable intent and history, but excludes the private CA,
controller identity and secret master key. Copying only a live database or
bootstrapping a new identity cannot recover an existing cluster safely.

## Decision

Expose local `cluster backup inspect` and `cluster restore` commands. Inspection
operates on a private temporary copy and reports aggregate inventory, original
and target schema versions, and recovery readiness. Validate SQLite integrity,
foreign keys and immutable migration history before forward migration. Reject
nonregular/private-file violations and any source SQLite sidecars. Never open
the original snapshot with a writable SQLite connection.

Restore requires matching, unexpired saved CA/controller credentials and any
master key recorded by the database, even when there are no secret versions.
Authenticate all encrypted versions, including historical and deleted records,
without exposing their values. Preserve the endpoint, cluster ID, CA and every
durable record; do not regenerate administrator or agent identities. The
existing backup command saves the master key whenever its fingerprint exists.

Prepare and verify all files in a private staging directory. Exclusively
create the destination and publish protected PKI/key files first, then the
checkpointed, synced database last using no-clobber links. Sync the destination
and parent. An error before publication leaves no destination; cleanup after a
normal publication error removes only the directory reserved by this restore.
A concurrent restore cannot overwrite or clean up another restore's directory.
After a crash, an incomplete directory without control.db cannot start; retry
into a fresh destination. No restore into an existing directory is supported.

## Boundaries

The old controller must be stopped or fenced before the recovered controller
starts. The operator restores endpoint routing, retains existing agent caches
and separately backs up administrator identity and local volume data. These
commands do not contact or stop production controllers.

A backup is a historical snapshot. Post-snapshot enrollment, certificate
rotation, assignments, secrets and job outcomes cannot be inferred from it.
Keep agent generation/digest fences intact; do not force stale assignments or
erase run tombstones. Completed jobs in the snapshot remain completed, and
unfinished jobs retain node ownership without automatic retry. This operation
does not merge divergent histories, guarantee exactly-once side effects,
recover expired PKI, or restore application data and runtime logs.

## Verification

Tests cover missing material versus readiness, wrong keys/identity, expired
certificates, authenticated ciphertext and malformed nonces, corrupt/foreign/
incompatible schemas, old-schema migration, foreign-key damage, private files,
sidecars, source immutability, destination refusal and concurrent publication.
A CLI/controller/agent loopback drill keeps the original operator and agent
identities, recovers the controller onto a fresh directory, reads secrets,
routes existing job logs, and replays a completed request without restarting it.
The runtime in that test is a fake; it is not a privileged Linux host drill.
