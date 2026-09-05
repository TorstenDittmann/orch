# ADR 0011: durable cron claims and availability-gated startup

## Status

Accepted

## Decision

`dependsOn` names services in the same application. The manifest rejects
unknown, duplicate, self, and cyclic dependencies. A dependency is available
when at least one authorized replica is RUNNING for process-only services or
READY for endpoint services. The gate applies before starts, restarts,
replacements, and rollbacks. A later dependency outage does not stop an
already-running dependent.

Top-level `cronJobs` are typed one-shot workload templates. Schedules use
exactly five UTC fields. Descriptors, seconds, and embedded timezone directives
are rejected. The agent durably claims deterministic occurrence IDs from its
cached assignment, permits no overlap or retry, waits for dependencies only
until `startDeadline`, enforces `timeout` after start, and retains the latest 20
runs per job. Missed historical occurrences are not replayed.

## Consequences

Schedules continue during controller outages while the owner agent has its
cached assignment. Sticky multi-node ownership makes graceful handoff wait for
active completion and old-node acknowledgement, while an unreachable owner
moves only after explicit confirmed fencing. Exactly-once execution is still
not implied by this contract.
