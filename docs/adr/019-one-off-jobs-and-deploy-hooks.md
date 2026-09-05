# 019: Controller-owned one-off jobs and pre-deploy hooks

## Decision

Reuse cron's isolated one-shot runtime path, but distinguish explicit runs from
schedules. A controller-generated run ID, scheduled time and immutable template
are persisted in `one_off_jobs`; a separate cancellation flag changes assignment
intent without changing the process spec. Agent protocol workloads-5 requires
coordinated controller/agent upgrades.

Each request is idempotent and bound to the selected service revision. Creation
checks the revision and resource reservation in a transaction. Accepted retries
return the same run; an explicit retry of an unsuccessful terminal run creates a
new identity and retains the original image, command and secret versions. Read
responses expose provenance and status without command/environment/secret data.

A run has fixed node ownership. Do not migrate or automatically retry it after
disconnection: the first process may have executed external side effects. Journal
the start attempt before calling the runtime. Recovery inspects the recorded
identity and reports an uncertain/missing outcome instead of invoking Ensure
again. Completion metadata is retained as a tombstone, while inactive completed
runtime/log objects are bounded to the newest 100 per agent.

Pre-deploy hooks refer to a service in the newly resolved manifest. Until their
zero exit code is durably recorded, reconciliation preserves the previous full
service assignment. Failure marks the deployment failed; explicit retry can
resume it. Superseding/removing a deployment requests hook cancellation. A newer
hook waits for the previous hook's terminal acknowledgement. Manual rollback is
a new revision and runs the source configuration's hook if present.

## Consequences

Hooks need spare resources and already-available dependencies. Jobs inherit
service resources, constraints, environment, secrets and dependencies but not
ports or volume mounts. There is no exactly-once guarantee for external database
side effects. Operators must use retry-safe migrations. A disconnected owner
must return to confirm termination; the controller does not infer termination
from a lost connection. Job metadata uses controller durability; logs retain the
existing agent-local availability limits.

## Verification

Store tests cover transactional admission, concurrent replay, resource limits,
immutable templates and restart persistence. Agent tests cover completion, exit
codes, cancellation, timeout and uncertain-start recovery without duplicate
starts. Controller tests cover authorization, replay, retry and failed/successful
hook gating. A loopback CLI/controller/agent test exercises runtime dispatch,
logs, exit codes, retry and cancellation with a fake runtime. TUI and typed Pkl
checks cover the operator workflow. Privileged runtime verification is reported
separately when performed.
