# Multi-node networking, discovery, and ingress

This is the implemented operator contract for one through 20 Linux nodes.
There is no virtual service IP, routing mesh, arbitrary
host-port publishing, NAT traversal, relay, or IPv6. Explicit raw TCP service
listeners are supported.

The request path stays out of the controller:

~~~text
internet -> ingress-node Caddy :80/:443 -> ready allocation IP:port
internet -> ingress-node TCP publicPort -> ready allocation IP:port
workload -> service.application.svc.internal -> ready local/remote allocation IP:port
node /24 -> WireGuard full mesh -> remote node /24
workload -> external IPv4 destination -> orch bridge -> masquerade
~~~

The controller distributes desired state and records observations. It is never
in a DNS or application request path.

## Host prerequisites

The agent validates these prerequisites and fails closed before attaching a
workload:

- Linux `amd64` or `arm64`, unified cgroups v2, containerd 2.3.4, and runc;
- `net.ipv4.ip_forward=1` and nftables kernel support;
- kernel WireGuard support, one canonical private IPv4 address reachable from
  every other node, and one available UDP listen port (51820 by default);
- the official CNI `loopback`, `bridge`, and `host-local` binaries from release
  1.9.1, supporting CNI 1.1.0, normally in `/opt/cni/bin`;
- Caddy 2.11.3 at the configured path on ingress-enabled nodes;
- a dedicated unprivileged ingress user and group on ingress-enabled nodes;
- ports 53 on the workload bridge; on ingress-enabled nodes, host TCP 80/443
  and every declared raw TCP public port must also be available.

The CNI directory and binaries and, where used, the Caddy binary must be
root-owned regular files, must not be group/world writable, and must not be
symlinks. CNI plugins
must be executable. Caddy must have only the host permission needed to bind
low ports, commonly `cap_net_bind_service=+ep`; orch refuses to launch it as
root.

For example, after installing the exact binaries by a distribution-appropriate
mechanism:

~~~console
setcap cap_net_bind_service=+ep /usr/bin/caddy
useradd --system --home-dir /var/lib/orch-ingress --shell /usr/sbin/nologin orch-ingress
install -d -m 0700 -o orch-ingress -g orch-ingress /var/lib/orch-ingress
sysctl net.ipv4.ip_forward
~~~

Account creation commands vary by distribution. Do not reuse the ingress
identity or data directory for another Caddy instance.

## CIDR selection and recovery

New clusters default to `10.88.0.0/16`. Before the first READY observation, an
administrator may choose a canonical RFC1918 IPv4 prefix from `/16` through
`/19`:

~~~console
orch network configure \
  --cidr 10.96.0.0/16 \
  --context "$HOME/.config/orch/context.json"
~~~

Every active node receives one stable, non-overlapping `/24`; its first usable
address is the bridge and DNS gateway. The original first node retains the
first `/24` through migration. For example, `10.96.0.0/16` gives the first node
`10.96.0.0/24` with gateway `10.96.0.1`. The remaining usable addresses come
from host-local IPAM. A subnet is reusable only after graceful removal or
explicit fenced lost removal.

Before mutation, the agent checks host interface addresses and routes. An
overlap is reported as `network_cidr_conflict`; orch does not create its
bridge, CNI lease, nftables rules, or Caddy process. Select a different prefix
with `orch network configure` and its printed idempotency key. Once the agent
reports the exact generation READY, the controller freezes the CIDR. A frozen
CIDR cannot be changed in place; migrate workloads to a new cluster instead of
manually editing either database or host-local leases.

## CNI, WireGuard, and firewall ownership

orch generates one canonical network list. Operators cannot inject plugin
JSON. A task is created first so its network namespace exists; CNI ADD and the
durable attachment record complete before the process starts. Failure runs
idempotent CNI DEL and removes only the matching orch-owned runtime object.

Names for the CNI network, bridge, and nftables table are deterministic hashes
of the verified cluster and node identities. The nftables table contains only:

- same-bridge workload traffic;
- assigned cluster-subnet traffic to and from the WireGuard interface;
- workload-to-external forwarding;
- established/related return traffic to the bridge; and
- source masquerade for traffic leaving the bridge.

New forwarded connections from external interfaces to allocation addresses
are dropped. The host and local Caddy process can reach those private
addresses. orch never flushes a foreign table, edits a foreign chain, changes a
global sysctl, or installs a `portmap` rule. Do not create unrelated resources
with an orch ownership name.

The bridge and shared table remain while the configured network is owned by the
agent. Orphan cleanup acts only on attachment state and containerd labels that
match the exact cluster, node, allocation, generation, and spec digest.

Each agent creates and protects a node-local WireGuard private key outside
SQLite with mode `0600`; only the public key is reported. Assignments contain
the exact key, canonical private endpoint, UDP port, and allowed `/24` of every
active peer. orch reconciles one ownership-scoped interface, its peer set,
routes, and nftables rules. Cluster-CIDR traffic goes through WireGuard without
masquerade, and peers may source only their assigned subnet. Foreign
interfaces, routes, keys, and firewall tables are never adopted or removed.

The hosts' canonical advertised private IPv4 addresses must already be
mutually reachable. Open the selected UDP port, 51820 by default, between every
pair of nodes in both host and provider firewalls. orch does not discover
public endpoints, punch through NAT, operate relays, or edit cloud firewalls.

## Workload DNS

The agent binds authoritative UDP and TCP DNS on port 53 of the bridge gateway.
Every known service has one exact name:

~~~text
service.application.svc.internal.
~~~

Responses contain sorted private IPv4 A records for current healthy allocations
across the whole cluster with a five-second TTL. The local agent persists that
versioned catalog before acknowledging it. AAAA returns NODATA. A known service
with no serving allocation returns NODATA; an unknown name below
`svc.internal.` returns NXDOMAIN.
Non-private names use a bounded set of host resolvers after removing the orch
gateway and forwarding loops. Queries, payloads, and response bodies are not
persisted or logged by default.

Each allocation receives a read-only generated `resolv.conf` pointing to the
gateway and using the application service zone as its search suffix.

## Readiness, updates, and DNS drain

Process phase and health are separate. For an HTTP service with `healthPath`,
the agent sends a direct GET to the allocation IP and declared port, sets Host
to `service.application.svc.internal`, rejects redirects, and accepts status
200 through 399. An HTTP service without a path and without public ingress or
domains uses a TCP connect. Attempts time out after one second and run every two seconds.
Two successes make the allocation READY; three failures make it UNHEALTHY.

Only serving addresses enter DNS and Caddy. A stateless update keeps the old
endpoint while its candidate starts and passes readiness. After one atomic
serving-snapshot and Caddy update, the old address is withdrawn and its task is
retained for at least the five-second DNS TTL before removal. Application-wide
`maxSurge` and `maxUnavailable` budgets gate concurrent replacements.

When `autoRollback = true`, an expired health deadline restores only the
previous spec explicitly authorized in the controller assignment. A local
volume update is stop-before-start, can have an availability gap, and does not
provide transactional data rollback. With rollback disabled, the failed
candidate remains withdrawn until a new application generation is submitted.

## Ingress roles, Caddy, and public routes

Ingress is an explicit node role. Every ingress-enabled node receives all
non-conflicting HTTP/HTTPS and raw TCP routes and proxies directly to ready
local or remote allocation IPs. Compute-only nodes do not need Caddy or public
listener availability. Route status is aggregated: a public application is
healthy when all desired allocations are ready and at least one eligible
ingress node has accepted its current route. A failure on another ingress node
remains visible as partial degradation.

Caddy receives deterministic native JSON containing exact validated domains
and cluster-wide READY private upstreams. Candidate configuration is validated
before the permissioned Unix admin API atomically loads it. The agent probes every HTTP
route and every HTTPS route with its configured SNI before accepting the new
fingerprint. Validation, load, or probe failure retains the last accepted
configuration.

`public = true` on an HTTP endpoint adds one plaintext catch-all route on port
80, allowing clients to use an ingress node's IP address directly. The
earliest-created application owns this cluster-wide listener; a later claimant
is `BLOCKED` with `http_public_conflict`. Exact domain routes are evaluated
before the catch-all, so domain-based HTTP and HTTPS continue to work alongside
IP ingress.

`https = true` is the default and uses Caddy automatic certificate management
plus an HTTP-to-HTTPS redirect. `https = false` creates an explicit port-80
route. Wildcards, IP literals, single-label names, embedded ports,
`svc.internal`, `localhost`, non-ASCII text, and non-canonical domains are
rejected. Cross-application conflicts are deterministic: the earliest created
application owns the exact domain, and a later claimant is `BLOCKED` with
`domain_conflict`.

For public HTTPS, the operator must:

- point every domain's public A records at one or more ingress-enabled nodes;
- allow inbound TCP 80 and 443 through host and provider firewalls on each;
- preserve every ingress node's data directory so its independent ACME account
  and certificate state survive restarts; and
- account for duplicate issuance and CA rate limits when adding or replacing
  ingress nodes, because certificate storage is node-local.

orch does not edit public DNS or cloud firewalls. Caddy operational logs are
protected and bounded; request/access bodies are disabled by default. The
admin API is a protected Unix socket, never a TCP listener.

## Raw TCP routes

`tcp = new Tcp { port = 9418; publicPort = 9418 }` binds the declared IPv4 host
port on every ingress-enabled node and forwards unmodified streams to
cluster-wide READY private replicas in round-robin order. Port 53 is reserved
for workload DNS and 80/443 for Caddy. Conflicts are
resolved globally by earliest application creation time; a later application
reports `tcp_port_conflict`. Removing or replacing a route stops new accepts
and gives existing connections 30 seconds to drain.

Raw TCP ingress does not open operating-system or cloud-provider firewalls.
There is no virtual IP or every-node routing mesh; operators direct clients to
one or more ingress-enabled node addresses.

Start the agent with explicit paths and identity:

~~~console
orch agent serve \
  --data-dir /var/lib/orch-agent \
  --advertise-address 10.0.0.11 \
  --wireguard-port 51820 \
  --containerd-socket /run/containerd/containerd.sock \
  --snapshotter overlayfs \
  --cni-bin-dir /opt/cni/bin \
  --caddy-binary /usr/bin/caddy \
  --ingress-user orch-ingress \
  --ingress-data-dir /var/lib/orch-ingress
~~~

An ingress role is enabled through the
`orch node ingress NODE --enable` administrator operation, not inferred from
workload constraints. The first enrolled node starts with ingress enabled for
upgrade compatibility; later nodes start compute-only.

## Status and stable failures

`orch nodes` reports lifecycle and connection separately, total/reserved/
assigned capacity, labels, node subnet, WireGuard mesh state, CNI spec and
plugin versions, nftables and DNS state, ingress role/state, and drain blockers.
It does not disclose configured filesystem paths or private keys.

`orch app status NAME` distinguishes desired and active generation/revision,
shows execution transition time, process phase, health, private address,
declared port, active spec, and each route's protocol/certificate/state/current
upstream count. Important meanings are:

- `RUNNING`: all exact desired processes are alive, for an application with no
  HTTP readiness contract;
- `HEALTHY`: every desired process and health gate is ready and every requested
  public route is active;
- `ROLLED_BACK`: the desired generation failed and an explicitly authorized
  earlier generation remains active;
- `STOPPING`: routes/DNS are withdrawn but the TTL drain or runtime cleanup is
  still in progress.

Common stable blockers include `cni_unavailable`,
`cni_plugins_unavailable`, `nftables_required`, `dns_unavailable`,
`network_cidr_conflict`, `wireguard_unavailable`, `wireguard_bind_failed`,
`caddy_unavailable`, and `domain_conflict`. Inspect the node and application
reason before changing host state.

Meaningful transitions emit deduplicated events such as `network.ready`,
`allocation.ready`, `allocation.unhealthy`, `route.active`, `route.failed`,
`application.healthy`, and `application.rolled_back`. Probe successes and
heartbeats do not create event noise.

## Outages, restart, removal, and backups

During controller or controller-network loss, every agent continues its cached
assignment, WireGuard peer set, probes, DNS catalog, ingress configuration,
cron schedules, local restarts, and running tasks. It cannot accept new desired
state, and no deployment, placement, drain, or rollout progression occurs. On
agent restart it reconciles that durable state, CNI attachment records,
containerd ownership labels, and the verified Caddy process identity; it never
adopts an unknown PID, socket, lease, route, interface, key, or task.

After a node is unreachable for 60 seconds, reachable agents receive a catalog
without its endpoints and stateless replicas may receive new placement attempts.
The unreachable agent still has its old cache, so a partition can leave
duplicate processes. A returning node must remove superseded attempts before
its endpoints are admitted again. Local-volume allocations and cron ownership
do not move automatically in this ambiguous state.

Application removal first withdraws DNS and routes, waits one DNS TTL, and then
deletes matching CNI leases and runtime objects. It preserves immutable
controller history, shared image content, Caddy account/storage used by other
domains, and declared local-volume contents. Foreign CNI, nftables, and
containerd objects are outside the cleanup boundary.

The controller database backup does **not** contain agent networking/health
state, Caddy certificates, the ingress account, workload logs, or local
volumes. Back up these three roots separately with suitable ownership and
encryption:

1. the controller data and PKI according to `docs/operations.md`;
2. `/var/lib/orch-agent`, while the agent is stopped for a consistent local
   snapshot; and
3. `/var/lib/orch-ingress`, preserving its dedicated UID/GID and modes.

Node-local CNI and WireGuard state is recovery metadata for that node, not a
portable cluster backup. Restoring it onto a different live host or alongside
a running agent is unsupported.

## Verification boundary

Portable unit, integration, race, generation, and cross-build checks run in
native change.codes CI. On a disposable compatible Linux host, run:

~~~console
mise install
mise exec -- make containerd-e2e
mise exec -- make network-e2e
~~~

Portable tests prove the assignment, endpoint-catalog, ownership, and recovery
state machines. Before a release, run the existing containerd and network E2E
commands on disposable compatible Linux hosts and additionally verify
cross-node encrypted traffic, global DNS, HTTP/raw TCP ingress, endpoint
withdrawal, drain/loss behavior, and preservation of foreign host networking.

These opt-in commands require host authority and may create real network
namespaces, a uniquely owned nftables table plus a foreign-preservation
sentinel, containerd tasks, DNS listeners, and Caddy listeners. `network-e2e`
starts the Pebble 2.10.1 tool pinned by
`mise.toml` on ephemeral loopback ports, uses an ephemeral trust root, and
proves trusted automatic HTTPS, redirect, and certificate reuse without
contacting a public CA. The commands are not silently replaced by fake success
on an unprivileged runner. The exact privileged and isolated-ACME results must
be attached as change.codes evidence for the revision being landed.
