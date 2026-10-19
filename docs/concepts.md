# Core concepts

This page explains the words orch uses. Each term has a short description, the
commands where it shows up, and a link to the page that covers it in depth. If
you are new to orch, read this page once, then follow the
[quickstart](quick-start.md).

orch deliberately exposes a small vocabulary: applications, services, revisions,
routes, secrets, and local volumes. It does not expose pods, scheduler objects,
or plugins. Most of the terms below are things you configure or inspect; a few
describe how orch works so that status output makes sense.

## At a glance

| Term | What it is | Where you see it |
| --- | --- | --- |
| Cluster | One controller plus up to 20 nodes | `orch status`, `orch nodes` |
| Controller | The single process that stores desired state and decides placement | `orch init`, `orch cluster ...` |
| Node (agent) | A Linux host that runs workloads | `orch join`, `orch node ...` |
| Ingress-enabled node | A node that accepts public HTTP, HTTPS, and TCP traffic | `orch node ingress NODE --enable` |
| Target | A remembered connection from your workstation to a controller | `orch target ...`, `--target` |
| Application | One `orch.pkl` file: a named group of services, cron jobs, and volumes | `orch deploy`, `orch app status` |
| Service | A long-running container definition inside an application | `services { ... }` in `orch.pkl` |
| Replica (allocation) | One running instance of a service on one node | `--replica N`, `orch app status` |
| Revision and deployment | An immutable stored version of an application and the record of rolling it out | `orch app history`, `orch rollback` |
| Plan | A local comparison of resolved application values | `orch plan` |
| Job, cron job, hook | One-shot runs: on demand, on a schedule, or before a rollout | `orch job ...`, `orch cron ...`, `deploy.preDeploy` |
| Secret | An encrypted value stored on the controller and referenced by name | `orch secret ...`, `secretEnv`, `secretFiles` |
| Volume | A local directory that stays on one node | `volumes { ... }`, `mounts { ... }` |
| Labels and constraints | Node key/value pairs and the exact matches a service requires | `orch node label`, `constraints { ... }` |
| Workload network | The private address range for containers, split into one `/24` per node | `orch network configure` |

## Cluster

A cluster is one controller and the nodes that have joined it. orch is built
for small clusters: one through 20 active nodes, up to 100 applications and 200
desired service replicas. Every node must be able to reach every other node
over IPv4, on private or public addresses.

You create a cluster once with `orch init` on the controller host and add
nodes with `orch join-token` and `orch join`:

```console
sudo orch init                     # on the controller
orch join-token worker-1             # prints the join command
sudo orch join orch1....           # on the worker
```

`orch status` shows the cluster name, address, workload network, and
certificate expiry. `orch nodes` lists the nodes.

See [installation](installation.md) and [limitations](product.md).

## Controller

The controller is the single authority for the cluster. It stores desired
state, revisions, secrets, and history in a SQLite database, decides where each
replica runs, and authorizes each step of a rollout. There is exactly one
controller; orch does not provide a highly available control plane.

The controller is not in the application data path. If it stops, nodes keep
running their last assigned workloads, routes, DNS, and cron schedules. New
deployments, placement changes, and cluster changes wait until it returns.

`orch init` installs it as the `orch-controller` systemd service. On the
controller host, root can run any `orch` command directly; from a workstation
you reach it through a [target](#target). Back it up with
`orch cluster backup`.

See [architecture](architecture.md), [cluster operations](operations.md#backups),
and [security and recovery](security.md#recovery).

## Node (agent)

A node, also called a worker, is a Linux host that runs workloads. Each node
runs the orch agent (`orch-agent`) and a dedicated containerd
(`orch-containerd`). The agent holds a durable copy of its assignment from the
controller, starts and supervises containers, runs health checks, configures
the node's network, answers service DNS, and runs ingress when enabled.

Each node has a unique name chosen when you create its join token. Nodes move
through lifecycle states that you control:

| State | Meaning | Command |
| --- | --- | --- |
| `ACTIVE` | Accepts new placements | `orch node uncordon NODE` |
| `CORDONED` | Keeps current work, accepts no new placements | `orch node cordon NODE` |
| `DRAINING` | Moves movable workloads elsewhere | `orch node drain NODE` |
| `REMOVED` | No longer part of the cluster | `orch node remove NODE` |

Whether a node is connected is reported separately from its lifecycle state.

See [installation](installation.md#workers) and
[node maintenance](operations.md#node-maintenance).

## Ingress-enabled node

An ingress-enabled node accepts traffic from outside the cluster. It runs Caddy
for HTTP and HTTPS routes and opens the listeners for raw TCP `publicPort`
routes. It forwards each request directly to a ready replica, on the same node
or across the WireGuard mesh.

The first node that joins a cluster is ingress-enabled automatically. Later
nodes are compute-only until you enable ingress:

```console
orch node ingress worker-2 --enable
orch node ingress worker-2 --disable
```

Point public DNS at the ingress nodes and allow TCP 80 and 443 (and any TCP
public ports) to them. Ingress is a node property, not a label; you do not
use constraints to choose ingress nodes.

See [networking](networking.md#https).

## Target

A target is a named connection from the CLI on your workstation to a
controller. Targets are stored in your user configuration directory, so you do
not pass credential paths on every command. A target reaches the controller
in one of three ways:

| Transport | How it connects |
| --- | --- |
| SSH socket | `orch target add NAME ssh://root@HOST` reaches the controller's local administrator socket over SSH. Your SSH key is the credential. |
| Direct | An imported administrator context connects to the controller's HTTPS address with mTLS. |
| WireGuard or SSH forward | `orch target transport NAME wireguard --address ...` or `... ssh --host ...` changes the route an imported context uses. |

The current target is used by default. Override it with `--target NAME` or the
`ORCH_TARGET` environment variable:

```console
orch target add production ssh://root@10.0.0.10
orch target list
orch target use production
orch target test
orch deploy --target staging
```

See [targets](targets.md) and
[administrator access](security.md#administrator-access).

## Application

An application is the unit you deploy. It is described by one Pkl file,
usually `orch.pkl`, that amends the versioned `v1alpha1/Application.pkl`
schema. An application has a name and contains
services, optional cron jobs, optional volumes, and a `deploy` block that
controls rollouts.

```pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "hello"

services {
  ["web"] { image = "ghcr.io/acme/hello:1.2.3" }
}
```

The schema is closed: unknown fields are errors, not ignored settings. Pkl is
evaluated on your workstation by the CLI. Only the resolved, canonical result
is sent to the controller; the controller and nodes never run Pkl.

Commands that work on an application file:

```console
orch validate        # evaluate and check the file
orch render          # print the resolved canonical form
orch plan            # list changes
orch deploy --wait   # submit it and wait for the rollout
```

Commands that work on a deployed application take its name:
`orch app status NAME`, `orch app history NAME`, `orch logs NAME SERVICE`,
`orch remove NAME`.

See [applications](configuration.md) and the [manifest reference](manifest.md).

## Service

A service is a long-running container inside an application. It names an OCI
image and, optionally, a command, environment, resources, restart policy,
dependencies, placement constraints, mounts, and at most one network endpoint:

- `http` for HTTP services, optionally with domains, HTTPS, and a health path;
- `tcp` for other TCP services, optionally published on ingress nodes
  (`publicPort`) or on the node that runs each replica (`nodePort`);
- `udp`, together with `http` or `tcp`, for node-local UDP ports.

A service without an endpoint is a background worker. Services in the same
application can wait for each other at startup with `dependsOn`.

Every service with an endpoint gets a cluster DNS name of the form
`SERVICE.APPLICATION.svc.internal`, which resolves to its ready replicas.

See [applications](configuration.md#services) and
[service discovery](networking.md#service-discovery).

## Replica (allocation)

A replica is one running instance of a service, placed on one node. Inside
orch it is called an allocation. Replicas are numbered from 1 and get their
own private address from the node's subnet.

`orch app status` lists each replica as `SERVICE/N` with its phase, health,
address, restart count, and the revision it is running. Commands that act on a
single instance take `--replica`:

```console
orch app status hello
orch logs hello web --replica 1 --tail 100
orch exec hello web --replica 1 -- /bin/sh
```

CPU and memory in `resources` are reserved on the node for each replica and
also enforced as limits. orch does not overcommit.

See [deployments](runtime.md#placement).

## Revision, deployment, and history

Every accepted change to an application creates an immutable revision. A
revision stores the resolved application, including the exact image digests
that tags resolved to at that moment. Submitting the same resolved
configuration again is a no-op.

A deployment is the rollout of one revision: orch starts replacements, waits
for readiness, and records the outcome. `orch deploy` returns `PENDING` as soon
as the controller has durably accepted the revision; `--wait` follows the
rollout until it is ready or fails.

| Command | Purpose |
| --- | --- |
| `orch app history NAME` | List revisions and their outcomes |
| `orch app diff NAME REVISION` | Compare a stored revision with current desired state |
| `orch app watch NAME` | Resume watching the current rollout |
| `orch rollback NAME REVISION --wait` | Deploy a stored revision again as a new revision |

A rollback reuses the stored image digests. It restores configuration and
secret references, not old secret values or volume data. With
`autoRollback = true` in the `deploy` block, a failed rollout returns to the
last healthy revision automatically.

See [deployments](runtime.md#rollbacks) and
[application status](operations.md#applications).

## Plan

A plan is a list of changes between two resolved applications, shown as
additions, modifications, and removals. `orch plan` evaluates your file on your
workstation. Without other options it lists the whole application as additions;
with `--against` it compares two files:

```console
orch plan
orch plan --against old.orch.pkl --file orch.pkl
```

Plans compare resolved values, so moving configuration between Pkl modules
does not show up as a change. `orch plan` does not contact the controller. To
compare against what is deployed, use `orch app diff NAME REVISION`.

See [applications](configuration.md).

## Jobs, cron jobs, and hooks

orch runs one-shot containers in three ways:

| Kind | Defined | Runs | Commands |
| --- | --- | --- | --- |
| One-off job | On the command line, from a service definition | When you ask | `orch job run`, `job show`, `job logs`, `job wait`, `job retry`, `job cancel`, `job list` |
| Cron job | `cronJobs { ... }` in `orch.pkl` | On a five-field UTC schedule | `orch cron runs`, `orch cron logs` |
| Pre-deploy hook | `deploy.preDeploy` in `orch.pkl` | Before a new revision replaces services | `orch deploy --wait`; a failed hook fails the deployment |

A one-off job uses a service's image, environment, secrets, and resources, but
not its ports or volumes:

```console
orch job run hello web -- ./bin/migrate up
orch job logs JOB_ID
```

A cron job has one owning node. Overlapping runs are skipped, and missed or
failed runs are not replayed. A pre-deploy hook must exit zero before the new
revision rolls out; until then the current services keep serving. None of
these guarantee exactly-once execution, so commands should be safe to retry.

See [jobs](jobs.md), [schedules](configuration.md#schedules), and
[one-off jobs](operations.md#one-off-jobs).

## Secret

A secret is a named, application-scoped value stored encrypted on the
controller. The application file references the name; the value never appears
in `orch.pkl`. Values are sent through stdin:

```console
printf '%s' "$DATABASE_URL" | orch secret set hello database-url --from-stdin
orch secret list hello
orch secret delete hello old-token
```

A service receives a secret as an environment variable (`secretEnv`) or as a
read-only file below `/run/secrets` (`secretFiles`). Setting a referenced
secret again rolls only the services and cron jobs that use it. A missing
referenced secret blocks the new revision while the previous one keeps
running.

Private registry credentials are a separate, cluster-scoped kind of secret,
managed with `orch registry login`, `list`, and `logout`.

See [secrets](secrets.md) and [security](security.md#secrets).

## Volume

A volume is a local directory on one node, declared in `volumes { ... }` and
mounted by a service with `mounts { ... }`. orch has only local volumes:

- a service that mounts a volume must have exactly one replica;
- the volume is claimed by the node where it first runs and never moves;
- if that node is lost, the service reports `volume_node_lost` and orch does
  not create an empty replacement elsewhere;
- a cron job may mount a service's volume read-only, for example for backups.

Removing an application or rolling back does not delete or restore volume
contents. Back up stateful data with tools suited to the application.

See [local volumes](configuration.md#local-volumes) and
[limitations](product.md).

## Labels and constraints

Labels are key/value pairs that you put on nodes. Constraints are the labels a
service or cron job requires. Matching is exact: a node is eligible only if it
has every requested label with the same value.

```console
orch node label worker-1 region=eu-west tier=compute
orch node unlabel worker-1 tier
```

```pkl
constraints {
  ["region"] = "eu-west"
}
```

If no node matches, the service is `BLOCKED` rather than placed somewhere
else. The `orch.*` label prefix is reserved. There are no affinity rules,
taints, or priorities; replicas of a service are spread across eligible nodes
by default.

See [placement](configuration.md#placement) and
[node labels](operations.md#node-labels).

## Workload network

The workload network is the private IPv4 range that container addresses come
from. The default is `10.88.0.0/16`. Each node owns one `/24` from it, and a
managed WireGuard full mesh routes traffic between node subnets. Every
replica has its own network namespace and address.

Choose a different range before the first workload if the default overlaps an
existing network. After that, the range is fixed:

```console
orch network configure --cidr 10.99.0.0/16
```

There is no virtual service IP or routing mesh. Cluster DNS returns the
addresses of ready replicas, and ingress nodes proxy directly to them.

See [networking](networking.md) and [architecture](architecture.md).

## Next steps

- Follow the [quickstart](quick-start.md) or the [tutorial](tutorial.md).
- Read how the pieces fit together in [architecture](architecture.md).
- Compare orch with other tools in [comparison](comparison.md).
