# ADR 027: CPU-based service autoscaling

## Status

Accepted. Narrows the autoscaling limit in the product scope.

## Context

Replica counts were fixed in the manifest, so absorbing load meant editing it
and deploying. Kubernetes' Horizontal Pod Autoscaler solves this with many
knobs, a metrics pipeline, and custom metrics. orch already sends node usage
with every 10-second heartbeat, and CPU requests are also CPU limits, so the
share of its request a replica uses is a bounded, comparable signal.

## Decision

A service may set `autoscale { min; max; cpuPercent }`. `replicas` is then
left unset and the canonical manifest records `min` as the starting count.

- **Signal.** Agents read each task's cumulative cgroup CPU time through
  containerd's task metrics and send the CPU each allocation used since the
  previous heartbeat. The controller keeps one minute of samples in memory;
  nothing is persisted, and agents without the field simply report none.
- **Decision.** Every 30 seconds the controller averages the samples of the
  service's ready replicas against the request and applies the HPA formula
  `ceil(current × utilization / target)` with a 10% tolerance, at most
  doubling per step and within `min` and `max`. Scale-up waits one window
  after a change. Scale-down waits until five minutes of evaluations all asked
  for fewer, then removes one replica. Nothing is decided while the
  application's current generation is still rolling out.
- **Capacity.** Before scaling up, the controller estimates how many replicas
  fit in unreserved CPU and memory on active, connected nodes that match the
  service's constraints, scales to that, and reports the service as limited
  instead of leaving unplaceable replicas blocked.
- **State.** The chosen count lives in `service_scales`, outside application
  revisions. The reconciler rewrites autoscaled services to that count in its
  in-memory copy of the manifest, so placement, rollouts, and readiness work
  unchanged. Scaling adds no deployment, keeps rollback targets, and records a
  `service.scaled` event. A deployment keeps the count within its new bounds.
- **Exclusions.** Services with local volumes or node-local ports cannot
  autoscale, since both are limited to one replica per volume or node.

## Consequences

Services absorb CPU load without a deploy, and operators see the count, CPU,
and limits in `orch app status`, the TUI, the dashboard, and metrics. Memory,
request-rate, and scheduled scaling are not provided, and orch still never adds
or removes nodes. After a controller restart, a service waits a full
scale-down delay before it shrinks.
