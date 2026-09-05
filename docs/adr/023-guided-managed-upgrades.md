# ADR 023: Guided managed upgrades

## Status

Accepted.

## Decision

Every orch binary exposes a machine-readable compatibility descriptor containing
its build identity, release generation, operating system, architecture, managed-install format,
controller and agent schema levels, exact agent protocol, and dependency-bundle
generation. Guided installers record this descriptor alongside content hashes.
Installations created before the descriptor are the managed generation-one
baseline.

`cluster upgrade` and `node upgrade` consume a trusted local candidate. They
validate the complete transition before mutation, acquire the existing install
lock, retain the previous executable, create a private state backup, and journal
each phase. The controller is upgraded before workers. A worker upgrade stops
only the agent so containerd tasks continue and can be adopted after reconnect.

Automatic rollback is permitted only before authenticated readiness. Controller
schema and agent-cache migrations are forward-only; after the new controller can
accept mutations or the agent acknowledges assignments, silently restoring old
state could discard accepted work. Later failures therefore stop with the exact
recovery path and require the documented operator procedure.

The first implementation replaces only the orch binary. A candidate with a
different managed dependency generation is rejected. Runtime-bundle upgrades,
downloads, release channels, remote fleet orchestration, downgrades, and
high-availability controller transitions require separate decisions.

## Consequences

Existing workloads remain available while a worker agent restarts. Exact
protocol gating can temporarily disconnect workers after a controller upgrade,
so the operator must complete workers sequentially before normal control-plane
operations resume. Recovery material consumes additional local disk and must be
retained until the upgraded cluster is verified.
