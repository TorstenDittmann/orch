# Single-node runtime contract

This document is the exact support boundary for the first executable orch
slice. It deliberately describes process execution, not a complete networked
application platform.

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
| Agent count | Exactly one enrolled node and one healthy live stream |
| Agent authority | Permission to access the containerd socket and create tasks, namespaces, snapshots, and cgroups |

The repository builds on macOS for development, but `agent serve` returns an
unsupported-platform error there. orch does not install, configure, or upgrade
containerd or runc in this slice.

## Executable application subset

Supported manifests use public, digest-qualified Linux OCI image references.
The runtime applies image defaults plus manifest command/args overrides, plain
environment variables, replica count, `always`/`on-failure`/`never` restart
policy, CPU quota, memory limit, and agent-managed local volume mounts.

The runnable example is [examples/runtime/orch.toml](../examples/runtime/orch.toml).
Its BusyBox image is a Docker Official Image multi-platform index pinned by
digest for amd64 and arm64. A digest is immutable; update it deliberately rather
than changing it through a tag.

Tag-only images, localhost/private registry requirements, secret references,
node constraints, and HTTP configuration are not partially applied. The
controller reports a stable `BLOCKED` reason for those recognized cases.

## Object ownership and content lifetime

The containerd namespace is `orch-<cluster-id>`. Namespaces prevent naming
collisions but are not trusted as an authorization boundary. Each runtime
object has an opaque deterministic ID and all of these labels:

- `io.orch.managed=true`
- `io.orch.cluster=<cluster-id>`
- `io.orch.node=<node-id>`
- `io.orch.allocation=<allocation-id>`
- `io.orch.spec-digest=<digest>`

The agent adopts, operates, and deletes an object only when its ID and every
ownership label match. A mismatch is an operational failure, not an invitation
to repair or delete an unknown object.

Image content and snapshots created for an allocation are attached to a
deterministic persistent containerd lease while assigned. Removal deletes the
stopped task, container, writable snapshot, and allocation lease. Shared image
content remains subject to containerd's normal references and garbage
collection. Retired objects' bounded log files are removed; declared local
volume directories are not.

## OCI isolation

Each workload receives new PID, mount, IPC, UTS, cgroup, and network
namespaces. The spec enables no-new-privileges, clears Linux capabilities,
exposes no host devices, and applies CPU and memory limits. It never mounts the
containerd socket, controller state, agent identity, or agent cache.

There is no CNI plugin or host networking. Do not rely on inbound, outbound,
service-to-service, DNS, or ingress connectivity. Any manifest containing an
HTTP block is `BLOCKED` until the networking and health-gate slice lands.

Only declared local volumes are bind-mounted, from
`<agent-data>/volumes/<application>/<volume>`. Paths are private directories;
targets are normalized and may not replace `/`, `/proc`, `/sys`, `/dev`, or
`/run/orch`. Local data is not replicated. It survives replacement and
application removal, but not loss of the host storage.

## Reconciliation and updates

The controller publishes complete ordered assignment snapshots with a positive
monotonic generation and digest. The agent commits a snapshot to SQLite before
ACK and never combines deltas. An ACK proves durable receipt only. Status moves
from controller-side intent to observed execution only after a matching report
for the exact acknowledged generation and digest.

The agent reconciles its last cached assignment while the controller or network
is unavailable. A process with matching ID and labels is adopted after agent
restart. Unchanged allocation spec digests continue running through application
updates.

Stateless replacement starts the new object before stopping the prior object
when `max_surge` permits it. Reconciliation is serialized, so this slice starts
at most one replacement at a time. With `auto_rollback`, a failed start retains
the prior process-running object. A local-volume replacement is always
stop-then-start and therefore has an explicit availability gap. The configured
`health_deadline` is a process-start deadline, not a readiness deadline.

Restart counts and next-attempt times are durable in the agent cache. Policies
use capped exponential backoff with bounded jitter. SIGINT or SIGTERM stops new
reconciliation and closes local/control state but intentionally leaves desired
tasks alive for adoption.

## Status meanings

| Status | Meaning |
| --- | --- |
| `PENDING` | No agent/capability report exists, or the current assignment is not durably acknowledged. |
| `BLOCKED` | Valid desired state cannot execute in this slice or a required reported capability is absent. |
| `STARTING` | The agent is creating or replacing the exact desired allocation set. |
| `RUNNING` | Every desired allocation process for the observed generation is alive. |
| `DEGRADED` | At least one desired process is running and another is stopped, failed, or backing off. |
| `FAILED` | No desired allocation is running under its current policy. |
| `STOPPING` | Desired state is tombstoned and removal has not yet been observed. |
| `REMOVED` | The agent acknowledged the tombstone and reports an empty desired allocation set. |

`RUNNING` is intentionally not `HEALTHY` or `READY`. No probe, route, or traffic
claim is made.

## Logs and exec

The agent stores stdout and stderr separately. Each stream has an 8 MiB current
file and one 4 MiB retained rotation, both mode `0600`. Rotation happens during
writes. `orch logs` returns at most 10,000 requested lines per stream as bounded
64 KiB protocol frames and does not follow new output. The controller forwards
frames and does not persist their contents.

`orch exec` requires an exact running allocation. It is non-interactive, has no
TTY or stdin, accepts 1–64 nonempty arguments totaling at most 8 KiB, emits
bounded 64 KiB frames, and runs for at most five minutes. It is never retried.
A nonzero remote exit is an operational CLI failure.

## Privileged runtime verification

The ordinary test suite uses an in-memory runtime at the containerd boundary.
On a dedicated Linux amd64 or arm64 host running containerd 2.3.4 with cgroups
v2, run the opt-in real-runtime suite as root:

~~~console
make containerd-e2e
~~~

Set `ORCH_CONTAINERD_SOCKET` or `ORCH_CONTAINERD_SNAPSHOTTER` when the host does
not use `/run/containerd/containerd.sock` and `overlayfs`. The runner uses the
tools pinned by `mise.toml`, builds a deterministic registry-free OCI fixture,
and creates a unique `orch-<cluster-id>` namespace. It verifies real task
creation, runc ownership metadata, cgroup settings, an empty network namespace,
separate stdout/stderr logs, bounded exec, restart, controller and agent
adoption, an unaffected allocation during update, volume persistence, and
resource cleanup. Its trap only cleans the UUID-shaped namespace written by the
test.

This privileged test is intentionally not presented as ordinary CI evidence.
Native change.codes CI runs all portable checks and compiles the gated test on
Linux; real-runtime evidence must come from a compatible dedicated host.

## Deliberate omissions

This slice has no networking, readiness/health checks, secrets, registry
credentials, node labels/constraints, multi-node scheduling, drain, failover,
agent certificate rotation/revocation, node replacement, installer ownership,
or automated restore. Those omissions are surfaced rather than silently
ignored.
