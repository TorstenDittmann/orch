# Runtime and placement contract

This document is the exact runtime boundary for the implemented one-through-20
node placement and WireGuard path.

## Host matrix

| Component | Supported contract |
| --- | --- |
| Operating system | Linux only |
| Architecture | `amd64` and `arm64` |
| Cgroups | Unified cgroups v2; `/sys/fs/cgroup/cgroup.controllers` must exist |
| containerd daemon/client | Exactly 2.3.4 |
| OCI runtime | `io.containerd.runc.v2`; both `containerd-shim-runc-v2` and `runc` must be on the agent's `PATH` |
| Reference runc bundle | runc 1.4.3 from the containerd 2.3.4 source fixture; other runc builds are not yet claimed as tested |
| Snapshotter | Explicitly selected with `--snapshotter` and available in the daemon; `overlayfs` is the documented default |
| Workload network | CNI spec 1.1.0 with the official loopback, bridge, and host-local plugins from release 1.9.1 |
| Host forwarding | IPv4 forwarding enabled and nftables available to the root agent |
| Ingress | On ingress-enabled nodes, Caddy 2.11.3 at an explicitly selected root-owned path, running as a dedicated non-root user |
| Agent count | 1–20 active enrolled nodes, one healthy live stream per node |
| Agent authority | Permission to access the containerd socket and create tasks, namespaces, snapshots, and cgroups |

The repository builds on macOS for development, but `agent serve` returns an
unsupported-platform error there. orch does not install or upgrade containerd,
runc, CNI plugins, or Caddy.

## Executable application subset

Supported manifests use public, digest-qualified Linux OCI image references.
The runtime applies image defaults plus manifest command/args overrides, plain
environment variables, replica count, `always`/`on-failure`/`never` restart
policy, CPU quota, memory limit, and agent-managed local volume mounts.

The runnable example is [examples/runtime/orch.pkl](../examples/runtime/orch.pkl).
Its BusyBox image is a Docker Official Image multi-platform index pinned by
digest for amd64 and arm64. A digest is immutable; update it deliberately rather
than changing it through a tag.

HTTP services may declare a private port, an optional readiness path, and
optional public domains. Exact node-label constraints and CPU/memory capacity
are hard placement requirements. Digest-qualified private registry images are
supported with an exact-host credential or anonymously when the registry
permits it. Missing referenced secrets block only that application's new
intent.

## Object ownership and content lifetime

The containerd namespace is `orch-<cluster-id>`. Namespaces prevent naming
collisions but are not trusted as an authorization boundary. Each runtime
object has an opaque deterministic ID and all of these labels:

- `io.orch.managed=true`
- `io.orch.cluster=<cluster-id>`
- `io.orch.node=<node-id>`
- `io.orch.allocation=<allocation-id>`
- `io.orch.spec-digest=<digest>`
- `io.orch.network-generation=<generation>`

The agent adopts, operates, and deletes an object only when its ID and every
ownership label match. A mismatch is an operational failure, not an invitation
to repair or delete an unknown object.

Image content and snapshots created for an allocation are attached to a
deterministic persistent containerd lease while assigned. Removal deletes the
stopped task, container, writable snapshot, and allocation lease. Shared image
content remains subject to containerd's normal references and garbage
collection. Retired objects' bounded log files are removed; declared local
volume directories are not.

## Runtime inventory and logs

The public runtime inventory is an allowlisted view of current desired service
allocations plus retained cron runs. It exposes logical identity, placement,
health/lifecycle state, resource reservations, revision/spec identity, exit and
restart state, and non-secret timestamps/reasons. It does not enumerate the
host containerd namespace or expose containerd object IDs, environment values,
registry credentials, or secret projection paths.

Log access resolves a service replica or exact cron run through controller
state and the assigned authenticated agent. The CLI receives a finite bounded
tail. The TUI retains at most 512 KiB and follows with an opaque cursor bound to
the active owned object and separate stdout/stderr offsets. Rotation, shrinkage,
replacement, and malformed or stale cursors produce a visible reset instead of
silently crossing runtime identities. Leaving a source cancels its operation.

## OCI isolation

Each workload receives new PID, mount, IPC, UTS, cgroup, and network
namespaces. The spec enables no-new-privileges, clears Linux capabilities,
exposes no host devices, and applies CPU and memory limits. It never mounts the
containerd socket, controller state, agent identity, or agent cache.

Each task is attached before process start to its node's stable `/24` within
the cluster workload CIDR (default `10.88.0.0/16`). The migrated first node
keeps the first `/24`. orch invokes the
official loopback plugin separately and uses a bridge plus host-local IPAM for
`eth0`. A uniquely named, ownership-marked nftables table permits forwarding,
return traffic, and outbound masquerading without modifying foreign tables.
Host-route or interface overlap fails closed as `network_cidr_conflict`.
Cluster-CIDR routes use an ownership-scoped WireGuard full mesh and are not
masqueraded; external IPv4 egress retains the existing masquerade behavior.

The agent generates a world-readable resolver file inside its private state
directory for each application and bind-mounts it read-only into that
application's containers. Its search suffix is
`<application>.svc.internal`, so both `service` and
`service.application.svc.internal` resolve through the DNS listener on the
bridge gateway. The agent serves authoritative A records below
`svc.internal.` over UDP and TCP and forwards other questions to the host
resolvers. Answers contain the cluster-wide catalog of current healthy local
and remote allocation addresses. Known services without ready addresses return
NODATA, and unknown private names return NXDOMAIN.

Only declared local volumes are bind-mounted, from
`<agent-data>/volumes/<application>/<volume>`. Paths are private directories;
targets are normalized and may not replace `/`, `/proc`, `/sys`, `/dev`, or
`/run/orch`. Local data is not replicated. It survives replacement and
application removal, but not loss of the host storage.

## Capacity, placement, and node loss

Agents report host CPU/memory and subtract bounded system reservations.
Allocatable CPU and memory are hard limits, with no overcommit. The controller
filters nodes by lifecycle, recent connection, runtime/network readiness, exact
labels, available capacity, and local-volume locality. It retains a feasible
current placement, then spreads replicas of the same service, then balances
the dominant remaining CPU/memory fraction with deterministic tie-breakers.
Healthy work is not moved merely to improve packing.

A logical replica has a stable application/service/replica identity. Each move
creates an immutable placement-attempt ID so an old report cannot overwrite a
replacement. Resource accounting includes desired attempts, retained rollback
objects, and authorized surge candidates.

After a node is unreachable for 60 seconds, stateless service attempts may be
replaced on healthy nodes. Its stale endpoints are withdrawn, but a partitioned
agent can keep running its cached processes; temporary duplicates are possible.
Returning nodes remove superseded attempts before re-entering discovery.
Local-volume allocations remain on their durable claimed node and report
`volume_node_lost` rather than starting with empty storage elsewhere.

## Reconciliation and updates

The controller publishes complete ordered assignment snapshots with a positive
monotonic generation and digest. The agent commits a snapshot to SQLite, brings
its runtime toward that snapshot, and only then ACKs; it never combines deltas.
Status moves from controller-side intent to observed execution only after a
matching report for the exact acknowledged generation and digest.

The agent reconciles its last cached assignment while the controller or network
is unavailable. A process with matching ID and labels is adopted after agent
restart. Unchanged allocation spec digests continue running through application
updates.

Stateless replacement starts the new object before stopping the prior object
when `maxSurge` permits it. Reconciliation enforces the application surge
budget and retains the serving revision until its replacement is ready. A
local-volume replacement is stop-then-start and therefore has an availability
gap. With `autoRollback`, three failed readiness probes or an expired health
deadline restores the controller-authorized previous spec; if the prior object
was stopped, the agent recreates it from the controller-issued assignment
snapshot.

Across nodes, the controller durably authorizes rollout progression so
application-wide `maxSurge` and `maxUnavailable` budgets still apply. Agents
execute only authorized transitions. A controller outage freezes placement and
rollout advancement; agents preserve the last accepted safe assignment,
including local restart and an explicitly authorized rollback.

Readiness runs every two seconds with a one-second timeout. An HTTP path uses a
direct HTTP GET with redirects disabled; without a path the gate is a TCP
connect. Two consecutive successes mark an allocation ready and three
consecutive failures mark it unhealthy. Only ready addresses enter public
routes. Caddy configuration is deterministic native JSON, validated before an
atomic admin-API load, locally probed, and rolled back if the route probe fails.

Restart counts and next-attempt times are durable in the agent cache. Policies
use capped exponential backoff with bounded jitter. SIGINT or SIGTERM stops new
reconciliation and closes local/control state but intentionally leaves desired
tasks alive for adoption.

## Status meanings

| Status | Meaning |
| --- | --- |
| `PENDING` | No eligible placement/capability report exists, or the current assignment is not durably acknowledged. |
| `BLOCKED` | A hard constraint, capacity, locality, or required capability is unsatisfied. |
| `STARTING` | The agent is creating or replacing the exact desired allocation set. |
| `RUNNING` | Every desired allocation process for the observed generation is alive. |
| `HEALTHY` | Every HTTP allocation passed readiness and every requested route is active. |
| `ROLLED_BACK` | A desired revision failed its health gate and the authorized previous revision is active. |
| `DEGRADED` | At least one desired process is running and another is stopped, failed, or backing off. |
| `FAILED` | No desired allocation is running under its current policy. |
| `STOPPING` | Desired state is tombstoned and removal has not yet been observed. |
| `REMOVED` | The agent acknowledged the tombstone and reports an empty desired allocation set. |

`RUNNING` remains the process-only success state for applications without HTTP.
For HTTP applications, rely on `HEALTHY` for the combined process, readiness,
and ingress claim.

## Logs and exec

The agent stores stdout and stderr separately. Each stream has an 8 MiB current
file and one 4 MiB retained rotation, both mode `0600`. Rotation happens during
writes. `orch logs` returns at most 10,000 requested lines per stream as bounded
64 KiB protocol frames and does not follow new output. The controller forwards
frames and does not persist their contents.

`orch exec` requires an exact current placement on a connected node. It is
non-interactive, has no TTY or stdin, accepts 1–64 nonempty arguments totaling
at most 8 KiB, emits bounded 64 KiB frames, and runs for at most five minutes.
It is never retried. A nonzero remote exit is an operational CLI failure.

## Privileged runtime verification

The ordinary test suite uses an in-memory runtime at the containerd boundary.
On a dedicated Linux amd64 or arm64 host running containerd 2.3.4 with cgroups
v2, run the opt-in real-runtime suite as root:

~~~console
mise install
mise exec -- make containerd-e2e
mise exec -- make network-e2e
~~~

Set `ORCH_CONTAINERD_SOCKET`, `ORCH_CONTAINERD_SNAPSHOTTER`, or
`ORCH_E2E_WORKLOAD_CIDR` when the host needs different values. The runner uses
the tools pinned by `mise.toml`, builds a deterministic registry-free OCI
fixture, and creates a unique `orch-<cluster-id>` namespace. It verifies real
task creation, runc ownership metadata, cgroup settings, network namespace
setup, application-scoped short-name DNS, outbound masquerading into an
isolated namespace, separate stdout/stderr logs, bounded exec, restart,
controller and agent adoption, preservation of an unaffected allocation, a
healthy rolling revision under continuous service traffic, an unhealthy
application-wide rollback, volume persistence, and resource cleanup. Its trap
only cleans the UUID-shaped namespace written by the test and the uniquely
named external test namespace. Foreign containerd, CNI-cache, and nftables
sentinels must remain untouched.

`containerd-e2e` requires the pinned Caddy binary and dedicated ingress user;
the agent starts it before freezing the network and adopts the same process
across controller and agent restarts. `network-e2e` runs that lifecycle first,
then adds real HTTP and HTTPS Caddy routes. It starts the Pebble version pinned
in `mise.toml` with an ephemeral loopback-only test CA, proves automatic
certificate issuance and HTTP-to-HTTPS redirect with a trusted client,
restarts Caddy, and verifies that the exact certificate is reused from durable
ingress storage. It never contacts a public ACME CA.

The portable suite covers deterministic multi-node placement, peer and route
reconciliation, global DNS/HTTP/TCP catalogs, drain, and loss behavior. A
privileged three-host or VM acceptance run is still required to validate the
complete kernel WireGuard and containerd data path on a release candidate.

The reference `BenchmarkScheduleTwentyNodesTwoHundredAllocations` result on an
Apple M3 Pro (`darwin/arm64`, Go toolchain pinned by `mise.toml`) is
0.569–0.638 ms/op across five runs. This is the in-process placement stage,
not a claim about agent or network convergence latency.

These privileged tests are intentionally not presented as ordinary CI evidence.
Native change.codes CI runs all portable checks and compiles the gated test on
Linux; real-runtime evidence must come from a compatible dedicated host.

## Deliberate omissions

The multi-node contract has no controller HA, distributed storage, automatic
host fencing, strong singleton execution, exactly-once cron, NAT traversal,
relay, virtual service IP, routing mesh, autoscaling, preemption, disk/GPU
scheduling, agent private-key rotation or general certificate revocation,
installer ownership, or automated restore. Those omissions are surfaced rather
than silently ignored.
