# ADR 0013: deterministic multi-node placement and direct WireGuard routing

## Status

Accepted and implemented by Plan #9; amended by ADR 024.

## Decision

orch supports one through 20 active Linux nodes behind one controller. The
controller remains the sole placement and rollout authority; agents execute
complete, durable assignments and keep the last accepted data plane running
when the controller is unavailable.

Agents report total CPU and memory and subtract bounded operator-selected
system reservations. Service CPU and memory requests are both scheduling
reservations and cgroup limits. Placement filters out ineligible lifecycle or
connection state, incompatible capabilities, exact label mismatches,
insufficient capacity, and conflicts with a durable local-volume claim. Among
feasible nodes it retains a healthy placement, spreads replicas of the same
service, balances the dominant remaining CPU/memory fraction, and uses stable
node-name and node-ID tie-breakers. There is no overcommit, preemption, or
background rebalance merely to improve packing.

Nodes have durable `ACTIVE`, `CORDONED`, `DRAINING`, and `REMOVED` lifecycle
states. Connection is a separate observation. Cordon preserves current work
but prevents new placements. Graceful drain evacuates movable allocations
within application-wide rollout budgets. Normal removal requires a connected,
drained, empty node to acknowledge its final empty assignment. Removing an
unreachable node requires the operator to assert both loss and external
fencing; orch cannot verify that the old machine is powered off. Removed node
certificates no longer authenticate.

Service and cron-job `constraints` are exact matches against bounded operator
labels. The `orch.*` label prefix is reserved for immutable system labels.
Ingress eligibility is a node property, not a placement label. Empty
constraints retain the existing `v1alpha1` application meaning and canonical
form.

Each active node owns one stable `/24` from the frozen cluster CIDR. A migrated
first node keeps its identity, certificate, workload objects, and first `/24`.
Every agent owns a mode-`0600` WireGuard private key outside SQLite and reports
only the public key. The controller distributes exact peer public keys,
canonical mutually reachable private IPv4 endpoints, UDP ports, and allowed
node subnets. The default listen port is UDP 51820. The bounded full mesh
routes cluster-CIDR traffic directly and without masquerade; existing external
egress remains masqueraded. orch provides no NAT traversal, relay, routing
mesh, virtual service IP, or user-supplied network configuration.

The controller distributes a canonical catalog of current healthy allocation
addresses. DNS on every node answers
`service.application.svc.internal` with healthy local and remote addresses.
Ingress-enabled nodes receive all non-conflicting HTTP/HTTPS and declared raw
TCP routes and proxy directly to allocation addresses through local routing or
WireGuard. Compute-only nodes do not require public listeners. Public DNS,
host/provider firewall rules, and mutually reachable node endpoints remain
operator responsibilities. Caddy ACME state is local to each ingress node.

Local volumes receive a durable node claim at first placement and never move.
Cordon leaves them running; drain reports `local_volume_pinned`; node loss
reports `volume_node_lost` and never creates an empty replacement elsewhere.

A cron template has one sticky owner. Graceful handoff waits for any active run
to complete and for the old owner to acknowledge removal. An unreachable
owner is not replaced until the operator confirms external fencing through
lost removal. This prevents automatic handoff in an ambiguous partition, but
does not claim exactly-once execution.

After 60 seconds unreachable, stateless service attempts may be replaced on
healthy nodes with new immutable attempt identities. The stale node's
endpoints are withdrawn from new catalogs, but its agent keeps executing its
last cached assignment. A partition can therefore temporarily leave duplicate
stateless processes. Returning nodes must acknowledge removal of superseded
attempts before their endpoints can re-enter discovery. Applications requiring
a singleton writer must provide external coordination or use pinned local
state; heartbeats are not fencing.

During a controller outage, existing tasks, local restarts, WireGuard routes,
DNS, ingress, and owned cron schedules continue from each agent's durable
state. New deployments, placement changes, drain progress, and rollout
progression pause. This is intentionally a single-controller availability
boundary, not control-plane high availability.

## Consequences

- The supported envelope is deliberately small: at most 20 active nodes, 100
  applications, and 200 desired service allocations.
- Exact constraints, requested CPU/memory, lifecycle, and volume locality are
  hard requirements. Unsatisfied intent is reported with a stable blocker
  rather than weakened silently.
- Replica spreading is a deterministic default, not an affinity or topology
  language. GPUs, disk, bandwidth, priorities, autoscaling, and preemption are
  not scheduled.
- Local storage is neither replicated nor portable. Labels do not turn local
  storage into distributed storage.
- Network partitions trade strict singleton execution for continued local
  operation. Only externally fenced lost removal makes cron reassignment safe
  enough to permit.
- A `/24` may be reused only after graceful removal or explicit fenced lost
  removal. Foreign interfaces, routes, and firewall tables remain outside
  orch's cleanup boundary.
- Controller and agents must be upgraded as a coordinated protocol change.
  Existing immutable assignment history remains readable and the first node
  is not recreated during migration.

## Rejected alternatives

- A generic scheduler API, affinity expressions, taints, and topology policies
  would expand the public abstraction beyond the needs of a 1–20 node cluster.
- A routing mesh or virtual service IP would add another data-plane layer when
  direct, encrypted allocation addressing is sufficient.
- Automatic movement of local volumes or cron jobs after a heartbeat timeout
  would imply storage replication or fencing that orch does not provide.
- Independent rollout decisions on agents could violate global surge and
  availability budgets; the controller therefore authorizes progression and
  freezes it during an outage.
