# orch compared with Docker Swarm and Kubernetes

This page helps you decide whether orch fits your workload, or whether Docker
Swarm, Kubernetes, or another orchestrator is a better choice. orch aims for
the simplicity of Docker Swarm with some of the modern features people
otherwise reach for Kubernetes to get: typed configuration, automatic HTTPS,
deployment history, and safe rollouts.

orch is young and deliberately narrow. Swarm and Kubernetes are mature,
widely deployed, and supported by large ecosystems. The comparison below
describes product behavior and design choices. It contains no performance
or setup-time measurements; those benchmarks have not been run yet.

## The short version

| If you need | Consider |
| --- | --- |
| A small Linux cluster run by one team, with HTTPS, rollbacks, and cron built in | orch |
| The Docker toolchain and Compose files you already have, on several hosts | Docker Swarm |
| A highly available control plane, broad APIs, operators, autoscaling, or distributed storage | Kubernetes |
| Kubernetes APIs with a smaller footprint | k3s |
| A general-purpose scheduler for containers and non-container workloads | Nomad |

## When to pick orch

orch is a good fit when most of these are true:

- You run between one and 20 Linux hosts (Ubuntu 24.04 LTS or Fedora 44 on
  amd64 or arm64 for guided installation) that can reach each other over IPv4.
- One team owns the cluster and its applications.
- Your applications are OCI containers: web services, workers, scheduled jobs,
  and the occasional single-node database.
- You want public HTTPS without installing and configuring an ingress
  controller and a certificate manager.
- You want deployments that are reviewable before they run, record their
  history, and can be rolled back to an exact earlier revision.
- You want configuration errors caught before deployment, including unknown
  fields and invalid combinations.
- A single controller is acceptable, as long as running applications keep
  serving when it is down.

What orch gives you out of the box, without add-ons:

- `orch init` and `orch join` to build a cluster, and SSH-based targets so
  there are no certificate bundles to copy around.
- Typed Pkl configuration with a closed schema; `orch validate` and
  `orch plan` run before anything reaches the cluster.
- Automatic HTTPS certificates on ingress nodes, plus raw TCP ingress and
  node-local TCP and UDP ports.
- An encrypted WireGuard mesh between all nodes and per-service DNS names.
- Image tags resolved to digests once per deployment and recorded, so
  retries and rollbacks run exactly the same images.
- Readiness-gated rolling updates, automatic rollback, deployment history,
  revision diffs, and manual rollback.
- Cron jobs, one-off jobs, and pre-deploy hooks for migrations.
- Encrypted secrets and registry credentials.
- A terminal UI, a local web dashboard, live logs, and events.

See [core concepts](concepts.md) and [architecture](architecture.md) for how
these fit together.

## When another tool is the better choice

Choose something other than orch when you need any of the following. These
are intentional limits, described in [limitations](product.md).

| You need | Why orch does not fit | Consider |
| --- | --- | --- |
| A highly available control plane | orch has exactly one controller. Running workloads continue during an outage, but deployments and cluster changes wait. | Swarm with several managers, Kubernetes, Nomad |
| More than 20 nodes | The WireGuard full mesh and placement are designed for 1–20 nodes, 100 applications, and 200 service replicas. | Kubernetes, Nomad, Swarm |
| Kubernetes APIs, Helm charts, or operators | orch has its own small API and no Kubernetes compatibility. | Kubernetes or k3s |
| Distributed or replicated storage | orch volumes are local to one node and never move. | Kubernetes with a CSI storage driver |
| Autoscaling | Replica counts are fixed in configuration. | Kubernetes |
| Automatic failover for stateful services | orch never recreates a local volume elsewhere and does not fence nodes automatically. | Kubernetes with replicated storage, or a managed database |
| Windows workers or other runtimes | orch runs Linux OCI containers through containerd and runc only. | Kubernetes, Nomad |
| Multiple teams with separate permissions | orch has administrator access only, without per-user roles. | Kubernetes |
| Multi-region control planes | orch has one controller; nodes in several regions are possible but share it. | Kubernetes federation tools, Nomad |
| Advanced placement rules | orch supports exact label matches and replica spreading only; there are no affinity rules, taints, or priorities. | Kubernetes, Nomad |
| An existing Compose and Docker workflow you do not want to change | orch uses Pkl, not Compose files, and does not use Docker Engine. | Docker Swarm |
| NAT traversal between nodes | Every node must reach every other node's address directly. | A VPN underneath orch, or another tool |

orch is also in early development. If you need a long support record, a large
community, and many third-party integrations today, Swarm or Kubernetes is the
safer choice.

## Feature comparison

The Swarm and Kubernetes columns describe their general, well-known behavior.
Exact details depend on version and, for Kubernetes, on the distribution and
add-ons you choose.

| Area | orch | Docker Swarm | Kubernetes |
| --- | --- | --- | --- |
| Setup | `orch init` on the controller, `orch join-token` and `orch join` per node. Guided installation installs pinned containerd, CNI plugins, and Caddy, and checks the host first. | `docker swarm init` and `docker swarm join` on hosts with Docker Engine installed. | Many options: managed services, kubeadm, or distributions such as k3s. A usable cluster also needs a CNI plugin and usually an ingress controller. |
| Config format | One Pkl file per application, amending a versioned schema. Supports modules, functions, and shared HTTPS imports. | Compose files deployed as stacks with `docker stack deploy`. | YAML or JSON manifests for many resource types, often templated with Helm or Kustomize. |
| Config validation | Closed, typed schema: unknown fields are errors. `orch validate` and `orch plan` run locally before deployment. | Compose files are checked against the Compose schema; some Compose options are not supported by Swarm. | The API server validates objects against their schemas; `kubectl diff` and server-side dry runs preview changes. |
| HTTPS and certificates | Built in. Ingress nodes run Caddy and request certificates automatically for configured domains. | Not built in for application traffic. Typically you run a reverse proxy such as Traefik or Caddy as a service. | Requires an ingress controller (or Gateway API implementation) and usually cert-manager. |
| Private networking | Built in. Each node owns a `/24`; a managed WireGuard full mesh encrypts all traffic between nodes. Service DNS returns ready replica addresses directly; no virtual IPs. | Overlay networks with built-in DNS and a virtual IP per service; overlay encryption is optional. A routing mesh publishes ports on every node. | Pod networking is provided by a CNI plugin of your choice. Services get virtual IPs and DNS names. Encryption depends on the plugin. |
| Control plane security | Mutual TLS between controller, agents, and administrators; one-time join tokens that pin the cluster CA. | Mutual TLS between nodes and join tokens, managed automatically. | TLS between components; authentication and RBAC for users and workloads. |
| Rollouts | Readiness-gated rolling updates with `maxSurge`, `maxUnavailable`, and a health deadline. | Rolling updates with parallelism, delay, order, and failure actions. | Deployment rolling updates with `maxSurge` and `maxUnavailable`, gated by readiness probes. |
| Rollback | Automatic rollback on failure, plus stored revision history, `orch app diff`, and `orch rollback` to any retained revision. | `docker service rollback` to the previous specification, and automatic rollback as an update failure action. | `kubectl rollout undo` to a previous ReplicaSet revision. Revisions are kept per Deployment. |
| Image pinning | Tags are resolved to digests by the controller once per deployment and recorded; retries and rollbacks reuse the recorded digests. | Tags are resolved to digests when a service is created or updated by default. | Images are pulled according to the pull policy; pinning to a digest is up to you or your tooling. |
| Secrets | Encrypted on the controller, referenced by name, delivered as environment variables or read-only files under `/run/secrets`. Changing a referenced secret rolls only the services that use it. | Encrypted in the Raft store and mounted as files under `/run/secrets`; secrets are immutable and rotated by creating a new one. | Secret objects, mounted as files or environment variables. Encryption at rest must be configured; external secret stores are common. |
| Jobs and cron | Built in: cron jobs with a five-field UTC schedule, one-off jobs from a service definition, and pre-deploy hooks that gate a rollout. | No built-in cron. One-shot tasks are possible with replicated-job services in recent versions; scheduled jobs need an external scheduler. | Job and CronJob resources. Migrations are often run as Jobs, init containers, or Helm hooks. |
| Stateful workloads | Local volumes only, for one-replica services, pinned to one node. Cron jobs can read them for backups. No replication or failover. | Named volumes are local to a node unless you use a volume plugin. | StatefulSets with persistent volumes from CSI drivers, including network and replicated storage. |
| Placement | Exact node label constraints, CPU and memory reservations, replica spreading. | Placement constraints and preferences, resource reservations, global and replicated modes. | Node selectors, affinity and anti-affinity, taints and tolerations, topology spread, priorities, and preemption. |
| Control plane availability | One controller with SQLite; workloads keep running during an outage; backup and restore commands. | Several managers with Raft consensus can tolerate manager failures. | Highly available control planes with several API servers and etcd members are standard. |
| Scale | 1–20 nodes, up to 100 applications and 200 service replicas. | Larger clusters than orch. | Much larger clusters. |
| Operator tools | CLI, terminal UI, and a local web dashboard in the same binary. | Docker CLI; third-party dashboards. | kubectl and a large choice of dashboards and tools. |
| Ecosystem | Small and new. No plugins by design. | Docker ecosystem and Compose tooling. | Very large: Helm charts, operators, CNI and CSI plugins, service meshes, and managed offerings. |

## How the designs differ

**One authority instead of consensus.** Swarm managers and Kubernetes control
planes replicate state with Raft (Swarm) or etcd (Kubernetes) to survive the
loss of a manager. orch uses one controller and one SQLite database. That
keeps backup, restore, and debugging simple, and it is the main reason orch is
not the right tool when the control plane itself must be highly available.
Agents keep a durable copy of their assignment, so applications, DNS, ingress,
and cron keep running during a controller outage. See
[when the controller is down](architecture.md#when-the-controller-is-down).

**Direct routing instead of virtual IPs.** Swarm's routing mesh and
Kubernetes services put a virtual IP in front of replicas. orch returns ready
replica addresses from DNS and has ingress nodes proxy directly to replicas
over WireGuard. There is one less layer, but clients that cache DNS answers for
a long time can hold on to an old address.

**Applications instead of resources.** Kubernetes exposes many composable
resource types. orch exposes one application file with services, cron jobs,
volumes, and a rollout policy, and resolves everything else itself. This
makes routine changes easy to review, and it means you cannot add new resource
types or controllers.

**Hard limits instead of best effort.** When a constraint, capacity
reservation, or volume claim cannot be satisfied, orch reports the service as
`BLOCKED` with a reason instead of placing it elsewhere. It never moves a local
volume, and it does not move cron jobs away from an unreachable node until you
confirm the node is fenced.

## Other alternatives

- **k3s** packages Kubernetes in a small distribution. It is a good choice
  when you want Kubernetes APIs and tools on small hardware. You still work
  with Kubernetes objects and choose ingress, certificates, and storage
  components.
- **Nomad** is an established client/server scheduler that runs containers and
  other workload types, with a highly available server cluster. It is the
  closest lightweight alternative. It is often paired with Consul and Vault
  for service networking and secrets.
- **Docker Compose** on a single host is simpler than any cluster. If one
  machine is enough, it may be all you need.

## Next steps

- Read the [limitations](product.md) before using orch in production.
- Learn the [core concepts](concepts.md).
- Set up a cluster with the [installation guide](installation.md) and
  [quickstart](quick-start.md).
