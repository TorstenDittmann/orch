# ADR 025: Read-only local volume mounts for cron jobs

## Status

Accepted. Amends ADR 0011 and ADR 0013.

## Context

Local volumes are pinned to one node by a durable claim and may only be used by
a one-replica service. Cron jobs could not mount volumes at all, so there was no
way to run a scheduled backup of a database's local data without the database
offering its own network dump endpoint.

## Decision

A cron job may declare `mounts`, but each mount must be `readOnly = true` and
must name a local volume that a service in the same application already mounts.
All of a job's mounts must belong to one service, so they share one node.

The volume claim remains created only by the owning service. The controller
places the cron template on the claimed node by passing the claim to the
scheduler as a hard volume constraint, exactly as for the service. Until the
service has been placed and the claim exists, the job is not assigned and the
application reports `cron_volume_pending`. Constraints and capacity still
apply; a job whose constraints exclude the volume node stays blocked with
`local_volume_pinned`.

Mounts are part of the cron spec digest (omitted when empty, so existing
digests do not change) and of the assignment protocol. The agent and runtime
reject a writable cron mount even if the controller sent one, and bind-mount
the volume directory with `ro`.

## Consequences

- Backups of Postgres, libSQL, SQLite and similar local data can run on a
  schedule next to the data and ship it elsewhere.
- Read-only prevents a run from corrupting or racing writes, but not from
  reading files mid-write. Jobs must use a copy method that is safe for the
  data, such as a database snapshot or backup command.
- Like any cron template, ownership is sticky. Adding mounts to a cron job that
  already runs on another node leaves it blocked until the owner is handed off,
  as with a constraint change.
- A volume no service mounts cannot be read by a cron job, since nothing would
  decide its node.

## Rejected alternatives

- Writable cron mounts would let a run write the same directory as the owning
  service with no coordination.
- Letting the cron job create the claim would allow a job to pin a volume
  before its service ever ran, and would make the job's constraints decide the
  service's node.
- Documenting network-only backups would make the capability depend on every
  application offering a dump endpoint.
