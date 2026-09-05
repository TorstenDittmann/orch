# ADR 0016: managed runtime explorer and incremental logs

## Status

Accepted by Plan #12.

## Context

The initial TUI exposed bounded log snapshots, but discovering them required
loading application detail and opening the first allocation. Cron logs always
selected the newest run, and nodes could not be used as a workload navigation
path. Exposing containerd directly would bypass the controller's desired-state
and ownership boundaries.

## Decision

The controller exposes a bounded, administrator-authorized inventory of
Orch-managed runtime identities: current service allocations and retained cron
runs. It joins desired identity and placement with agent observations while
allowlisting fields safe for operator display. Containerd object IDs, arbitrary
host containers, desired environment, and secret material remain private.

The TUI presents this inventory as one responsive runtime workspace, groupable
by application or node. A selected service replica or exact cron run is the
only source for its log pane. Wide terminals use tree/detail/log panes; narrow
terminals retain the selection and use tree/log screens.

Log continuation is an opaque cursor bound by the agent to the resolved owned
runtime object and per-stream positions. Existing CLI requests without a cursor
remain finite tails. The TUI performs bounded continuation reads, follows only
while at the bottom, and reports exit, truncation, rotation/reset, replacement,
and connectivity failures. The controller never persists workload log data.

## Consequences

Runtime navigation no longer depends on an implicit first replica or latest
cron run and behaves the same through direct, WireGuard, and SSH targets.
Incremental logs add cursor compatibility and cancellation tests across the
runtime, agent, controller, client, and TUI. The workspace is deliberately
read-only; restart, stop, delete, exec, and a browser dashboard require separate
product and authorization decisions.
