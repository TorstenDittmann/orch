# ADR 0017: deployment history, manual rollback, and rollout watching

## Status

Implemented in Change #27; scope recorded in Plan #13 following the user's
implementation request.

## Context

Application revisions are immutable, but execution observations previously
belonged only to the application's latest desired state. Operators could
inspect runtimes and automatic rollback but could not browse prior deployment
outcomes or deliberately restore a known revision. Asynchronous acceptance also
left scripts without a generation-specific readiness result.

## Decision

Keep immutable canonical revisions and add per-revision deployment records in
migration 009. Record current execution changes in the same transaction as the
application observation. Retain the first terminal rollout outcome separately
from subsequent health observations. Close unfinished revisions on supersession
or removal. Backfill historical outcomes as unknown; preserve only what the
current execution state can establish.

Expose administrator-only history, exact revision metadata, semantic comparison,
and rollback through the existing Connect API. Use exclusive generation cursors,
record and byte bounds, deterministic ordering, and value redaction. Comparison
reads stored resolved intent and does not reevaluate configuration or secrets.

Rollback is an explicit generation-guarded application apply: read an immutable
source, validate it against the supported configuration contract, then atomically
check expected desired generation, enforce capacity, append a new revision,
record the source, and persist the exact idempotency response. Replay precedes
current-generation checks. The controller never resets generation counters,
rewrites source revisions, reactivates removed apps, or bypasses reconciliation.

Keep asynchronous apply's PENDING receipt. CLI waits are cancellable observation
loops for a fixed application/generation/revision. Readiness requires matching
observed and active intent; failure, automatic rollback, supersession, and
removal cannot be mistaken for success. Cron schedules require acknowledged
matching assignments and observations, without waiting for an actual run.

The TUI uses the same client and generation guard. Its paged workspace preserves
selection by revision ID, rejects late reads by request generation, marks stale
snapshots, and requires a target-specific confirmation. An uncertain mutation
response retains the original retry identity. Narrow layouts preserve access to
comparison and progress through a scrollable detail screen.

## Consequences

Older outcomes cannot be reconstructed reliably. Successful deployment records
may coexist with a later unhealthy observation; the record distinguishes these.
Rollback restores configuration and current secret references, not historical
secret values or stateful data. Missing dependencies may block a rollback.

Migration 009 is forward-only, consistent with the existing SQLite policy.
Upgrade controller and CLI together to use the added RPCs. Older asynchronous
apply and log clients retain their contracts. No new agent wire fields or
transport-specific paths are introduced.
