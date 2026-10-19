# Architecture

This page describes how orch works inside: which processes run where, how they
talk to each other, how traffic reaches your containers, and what happens
during a deployment or an outage. You do not need it to deploy an application,
but it explains the behavior you see in `orch status`, `orch app status`, and
the [limitations](product.md).

The design favors a small number of moving parts. There is one controller, one
agent per node, a dedicated containerd on each node, Caddy on ingress nodes,
and a WireGuard mesh between nodes. The decisions behind each part are
recorded as architecture decision records (ADRs) in `architecture/adr/` in the
repository.

## Overview

```text
  Workstation                         Controller host
  +-------------------+   SSH or      +-------------------------------+
  | orch CLI / TUI /  |---mTLS------->| orch controller               |
  | orch web          |   HTTPS       |  - SQLite desired state       |
  | Pkl evaluation    |               |  - placement and rollouts     |
  +-------------------+               |  - secrets (encrypted)        |
                                      |  - tag -> digest resolution   |
                                      +---------------+---------------+
                                                      ^
                       agents dial the controller and | mTLS stream:
                       keep a durable copy of their   | assignments down,
                       assignment                     | reports up
            +-----------------------------------------+---------------+
            |                                                         |
  +---------+--------------------+               +--------------------+---------+
  | Node A (ingress-enabled)     |   WireGuard   | Node B (compute-only)        |
  |  orch agent                  |<=============>|  orch agent                  |
  |  orch-containerd + runc      |  full mesh,   |  orch-containerd + runc      |
  |  Caddy (HTTP/HTTPS)          |  UDP 51820    |  service DNS on 10.88.1.1    |
  |  TCP ingress listeners       |               |  nftables                    |
  |  service DNS on 10.88.0.1    |               |  replicas in 10.88.1.0/24    |
  |  nftables                    |               +------------------------------+
  |  replicas in 10.88.0.0/24    |
  +------------------------------+
            ^
            | public HTTP, HTTPS, TCP
         Clients
```

The controller is on the control path only. Application traffic flows from
clients to ingress nodes and then directly to replicas, locally or over
WireGuard. It never passes through the controller.

## Components

| Component | Runs on | Responsibility |
| --- | --- | --- |
| `orch` CLI, TUI, and `orch web` | Your workstation or the controller host | Evaluates Pkl, sends resolved applications, reads status, history, and logs |
| Controller (`orch-controller`) | One host | Stores desired state, resolves image tags, places replicas, authorizes rollouts, encrypts secrets, issues certificates |
| Agent (`orch-agent`) | Every node | Applies its assignment, supervises containers, runs health checks, configures networking, serves DNS, runs ingress |
| containerd (`orch-containerd`) and runc | Every node | Pulls OCI images and runs containers |
| WireGuard interface | Every node | Encrypted full mesh between node subnets |
| Caddy | Ingress-enabled nodes | HTTP and HTTPS routing with automatic certificates |

All orch roles are subcommands of one `orch` executable. Caddy, containerd,
and runc remain separate, pinned processes that orch installs and supervises.

## Controller

There is exactly one controller per cluster (ADR 0005). It keeps all durable
state in a single SQLite database in write-ahead-log mode with full
synchronous writes: nodes, applications, immutable revisions, deployment
records, placements, encrypted secrets, join tokens, jobs, and events. Schema
migrations are forward-only.

A single controller removes consensus from everyday operation and makes backup
and restore a matter of one database snapshot plus key material. The cost is
that orch has no highly available control plane. See
[when the controller is down](#when-the-controller-is-down).

The controller serves its API on two listeners:

- an HTTPS port (9443 by default) that requires mutual TLS for administrators
  and agents, plus a separate certificate-free enrollment service for joining
  nodes;
- a local Unix socket for administrators, which only root and the controller
  account can open. `orch target add NAME ssh://root@HOST` reaches this socket
  over SSH.

The controller never runs Pkl. The CLI evaluates `orch.pkl` on your
workstation and sends only the resolved canonical JSON (ADR 0008). The
controller still validates that input as untrusted.

## Agents and the container runtime

Each node runs one agent. The agent opens a mutually authenticated,
bidirectional stream to the controller, reports its capacity and status, and
receives a complete assignment: the replicas and cron jobs it should run, the
node's network configuration, its WireGuard peers, the catalog of healthy
endpoints, and its ingress routes. The agent stores the assignment durably
before acknowledging it, so it can restart while disconnected and continue
from the same state.

The agent runs a local control loop that compares the assignment with what is
running and converges the two. It sends a heartbeat every 10 seconds.

Containers run through a dedicated, pinned containerd and runc installation,
not Docker Engine (ADR 0004). orch uses its own containerd namespace and
labels every object it owns, and it refuses objects whose ownership labels do
not match. Each container gets:

- its own network, PID, mount, and other namespaces;
- no Linux capabilities and `no-new-privileges`;
- CPU and memory limits equal to the service's reserved `resources`;
- only the bind mounts orch manages (volumes and secret files).

A worker upgrade restarts only the agent. Containers keep running under
containerd and are adopted again when the new agent starts. The controller
starts worker upgrades itself after its own upgrade: it sends each older agent
an upgrade command naming its binary's SHA-256, one node at a time, and the
agent downloads that binary from the controller over its mTLS connection.

See [runtime requirements](runtime.md) and [installation](installation.md).

## Networking

### Workload network and node subnets

The cluster has one workload network, `10.88.0.0/16` by default. It becomes
fixed once workloads exist. Each active node owns one stable `/24` from it
(ADR 0006, ADR 0013). On each node the agent creates a CNI bridge, assigns
replica addresses from the node's `/24`, and installs nftables rules in its own
table. Each replica has its own network namespace and private address.

### WireGuard mesh

Every node generates its own WireGuard private key and reports only the public
key. The controller distributes each node's peers: public key, advertised IPv4
address, UDP port (51820 by default), and node subnet. The result is a full
mesh: every node routes directly to every other node's `/24`, with no relay and
no masquerading of cluster traffic. Outbound traffic to the internet is still
masqueraded.

Advertised addresses can be private or public (ADR 024). Only the WireGuard
underlay uses them; workload addresses, DNS answers, and ingress upstreams stay
inside the private workload network. orch provides no NAT traversal, so every
node must reach every other node's advertised address directly. The full mesh
is one reason for the 20-node limit.

### Service DNS

The controller distributes a catalog of healthy replica addresses to all
nodes. Each agent serves DNS on its subnet's gateway address, port 53, for the
`svc.internal` zone and forwards other names to the host's resolvers. A name
such as `database.shop.svc.internal` returns the addresses of the ready
replicas of service `database` in application `shop`, wherever they run.

There is no virtual service IP and no routing mesh. Clients connect directly to
replica addresses, and replicas leave DNS answers when they stop being ready.

See [service discovery](networking.md#service-discovery).

### HTTP and HTTPS ingress

Ingress-enabled nodes run a pinned Caddy as a dedicated non-root user (ADR
0007). The agent renders Caddy's configuration from the application routes and
the healthy endpoint catalog, validates it, and reloads it atomically through a
protected admin socket. Caddy obtains and renews HTTPS certificates
automatically. Certificate state is local to each ingress node, so each
ingress node needs DNS and ports 80 and 443 to work for issuance.

A route is published after its service becomes healthy, and Caddy proxies only
to ready replicas.

### Raw TCP ingress

A service with `tcp.publicPort` gets a listener on every ingress-enabled node
(ADR 0010). The agent forwards bytes to ready replicas with round-robin
selection and no protocol inspection. When a route is withdrawn, the listener
stops accepting immediately and existing connections get 30 seconds to finish.
A public port belongs to the earliest application that claims it; later
applications are `BLOCKED` with `tcp_port_conflict`.

### Node-local ports

A service with `tcp.nodePort` or `udp.nodePort` is published only on the node
that runs each replica (ADR 026). The agent adds destination NAT rules to its
nftables table, so there is no user-space proxy and the container sees the
client's address. The scheduler treats each published port as a node resource.

See [networking](networking.md) for configuration and firewall requirements.

## Scheduling and placement

The controller is the only scheduler (ADR 0013). Agents report total CPU and
memory, minus the system reservation chosen at installation. Placement then
works in two steps.

First, it filters out nodes that cannot run the replica:

- nodes that are not `ACTIVE` or not connected;
- nodes missing a required capability;
- nodes that do not have every label in the service's `constraints`;
- nodes without enough unreserved CPU and memory for the request;
- nodes that conflict with a local volume claim or a node port already in use.

Second, among the remaining nodes it keeps a replica where it already runs
healthily, spreads replicas of the same service across nodes, balances the
remaining CPU and memory, and breaks ties by node name and ID. The result is
deterministic.

There is no overcommit, preemption, priority, or background rebalancing. When
no node fits, the service is `BLOCKED` with a stable reason such as a label
mismatch or missing capacity, instead of being placed somewhere weaker.

Local volumes get a durable node claim on first placement and never move.
Cron jobs have one sticky owner node.

### Node failure

When a node has been unreachable for 60 seconds, stateless replicas may be
replaced on healthy nodes, and the lost node's endpoints are removed from new
DNS and ingress catalogs. The unreachable agent keeps running its last
assignment, because orch cannot tell a dead node from a partitioned one. A
partition can therefore leave duplicate stateless processes for a while. When
the node returns, it must acknowledge removal of the superseded replicas
before its endpoints are used again.

Volumes and cron jobs do not move automatically. A service with a local volume
reports `volume_node_lost`. Cron ownership moves only after a graceful handoff,
or after you remove a lost node with `orch node remove NODE --lost
--confirm-fenced`, asserting that the old machine cannot run workloads.

See [placement](runtime.md#placement) and
[node maintenance](operations.md#node-maintenance).

## Deployment flow

A deployment goes from a Pkl file on your workstation to running replicas in
these steps:

```text
 orch.pkl --(evaluate on workstation)--> canonical JSON
          --(orch deploy)--> controller: validate, resolve tags to digests,
                             store immutable revision, reply PENDING
          --> optional pre-deploy hook must exit 0
          --> controller places replicas and authorizes rollout steps
          --> agents start replacements, health checks pass,
              DNS and ingress switch to ready replicas, old replicas stop
          --> deployment recorded HEALTHY, or FAILED (and auto-rolled back)
```

1. **Evaluate.** `orch validate`, `render`, `plan`, and `deploy` evaluate the
   Pkl module locally, apply the schema and validation rules, fill in defaults,
   and produce canonical JSON with a digest. `orch plan` compares resolved
   values locally and does not contact the controller.
2. **Accept.** `orch deploy` sends the canonical JSON with an idempotency key.
   The controller validates it again and resolves each image tag to an
   immutable digest by contacting the registry (ADR 018). If resolution
   fails, the whole request is rejected and desired state is unchanged. The
   controller stores the authored request and the resolved revision, then
   replies `PENDING`. Submitting the same resolved configuration again is a
   no-op, and retrying with the same idempotency key returns the original
   answer.
3. **Hook.** If the `deploy` block defines a `preDeploy` hook, it runs as a
   one-shot container from the new revision's service definition. Current
   services keep their previous assignment until the hook's zero exit code is
   recorded. A failing hook fails the deployment (ADR 019).
4. **Roll out.** The controller places replicas and authorizes each step
   within the application's `maxSurge` and `maxUnavailable` budget. Agents pull
   the recorded digests, start new replicas, and run health checks every two
   seconds: an HTTP GET to `healthPath`, or a TCP check. Only ready replicas
   enter DNS and ingress. Old replicas stop as replacements become ready.
5. **Record.** The deployment record keeps the outcome. A replacement that
   does not become ready within `healthDeadline` fails the rollout. With
   `autoRollback` enabled, replicas return to the last healthy revision.

`orch deploy --wait` and `orch app watch` follow a specific revision, so a
failure, automatic rollback, or newer deployment is never reported as success.

### Rollback

A manual rollback (`orch rollback NAME REVISION`) reads a stored revision and
appends it as a new revision (ADR 0017). It reuses the stored image digests
and never contacts the registry, so nodes must still be able to pull those
digests from a cache or the registry. It runs the source revision's
pre-deploy hook if it has one. It restores configuration and current secret
references, not historical secret values or volume data.

See [deployments](runtime.md) and [jobs](jobs.md).

## Secrets

Secret values are stored on the controller as immutable versions encrypted with
AES-256-GCM (ADR 0012). The key-encryption key is a separate mode `0600` file
outside the database, which is why backups write it to a separate output.

The durable assignment sent to a node contains only references. The secret
values travel in a separate, transient bundle over the authenticated agent
stream, and the agent re-encrypts them with its own local key before storing
them. In the container, secret files are mounted read-only from a root-only
tmpfs. Secret environment variables are read from that tmpfs by a small
initializer just before it starts the workload process, so they are not stored
in the container specification.

Registry credentials are stored the same way and are used only when pulling
from the exact matching registry host over HTTPS.

See [secrets](secrets.md) and [security](security.md#secrets).

## Certificates and enrollment

orch runs its own certificate authority. The CA is valid for ten years, and
the controller and administrator certificates for one year. Controller, agent,
and administrator traffic uses mutual TLS with separate credentials for each
role.

Enrollment works like this (ADR 0014):

1. `orch join-token NAME` (or `orch node join create`) generates a random
   secret on the operator's machine and builds a token bound to one node name,
   the cluster ID, the controller address, the CA fingerprint, and an expiry.
   The controller stores only a hash of the secret.
2. `orch join` on the new node checks the host, installs the pinned
   dependencies, creates a private key, and calls the enrollment service. It
   trusts only the CA pinned in the token.
3. The controller checks the token, consumes it, registers the node, and signs
   its certificate in one transaction. A retried request with the same token
   gets the same answer; a different request is rejected.

Workers never receive administrator credentials. Agents renew their own
certificates automatically during the last 90 days, keeping the same key,
node ID, subnet, and placements. Controller and administrator certificates do
not renew automatically; see
[certificate renewal](security.md#certificate-renewal).

## When the controller is down

The controller is a single point of control, not a single point of failure
for running applications (ADR 0005, ADR 0013). While it is unavailable:

| Keeps working | Waits for the controller |
| --- | --- |
| Running replicas and their local restarts | New deployments and rollbacks |
| WireGuard routes between nodes | Rollout progress and drain progress |
| Service DNS with the last known endpoints | Placement changes and node replacement |
| HTTP, HTTPS, and TCP ingress with the last accepted configuration | Node, label, and ingress changes |
| Cron schedules owned by each node | Secret changes and new one-off jobs |

Agents reconnect when the controller returns. To recover from losing the
controller host, restore a backup onto a new host at the same HTTPS address
with `orch cluster restore` (ADR 020). See
[controller outages](operations.md#controller-outages) and
[recovery](security.md#recovery).

## Where to go next

- [Core concepts](concepts.md) for the vocabulary.
- [Networking](networking.md) for ports, DNS, and ingress configuration.
- [Security and recovery](security.md) for credentials and backups.
- [Comparison](comparison.md) for how this design differs from Docker Swarm
  and Kubernetes.
