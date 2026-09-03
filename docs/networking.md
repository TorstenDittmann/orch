# Single-node networking, discovery, and ingress

This is the operator contract for orch's first networked application slice.
It covers one Linux node only. There is no overlay, WireGuard peer, virtual
service IP, routing mesh, arbitrary host-port publishing, or IPv6 in this
release. Explicit raw TCP service listeners are supported.

The request path stays on the node:

~~~text
internet -> Caddy :80/:443 -> ready allocation IP:port
internet -> declared TCP publicPort -> ready allocation IP:port
workload -> service.application.svc.internal -> ready allocation IP:port
workload -> external IPv4 destination -> orch bridge -> masquerade
~~~

The controller distributes desired state and records observations. It is never
in a DNS or application request path.

## Host prerequisites

The agent validates these prerequisites and fails closed before attaching a
workload:

- Linux `amd64` or `arm64`, unified cgroups v2, containerd 2.3.4, and runc;
- `net.ipv4.ip_forward=1` and nftables kernel support;
- the official CNI `loopback`, `bridge`, and `host-local` binaries from release
  1.9.1, supporting CNI 1.1.0, normally in `/opt/cni/bin`;
- Caddy 2.11.3 at the configured path;
- a dedicated unprivileged ingress user and group;
- ports 53 on the workload bridge and host ports 80 and 443 available to the
  managed processes.

The CNI directory and binaries and the Caddy binary must be root-owned regular
files, must not be group/world writable, and must not be symlinks. CNI plugins
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

The one node receives the first `/24`; its first usable address is the bridge
and DNS gateway. For example, `10.96.0.0/16` produces node subnet
`10.96.0.0/24` and gateway `10.96.0.1`. The remaining usable addresses come
from host-local IPAM.

Before mutation, the agent checks host interface addresses and routes. An
overlap is reported as `network_cidr_conflict`; orch does not create its
bridge, CNI lease, nftables rules, or Caddy process. Select a different prefix
with `orch network configure` and its printed idempotency key. Once the agent
reports the exact generation READY, the controller freezes the CIDR. A frozen
CIDR cannot be changed in place; migrate workloads to a new cluster instead of
manually editing either database or host-local leases.

## CNI and firewall ownership

orch generates one canonical network list. Operators cannot inject plugin
JSON. A task is created first so its network namespace exists; CNI ADD and the
durable attachment record complete before the process starts. Failure runs
idempotent CNI DEL and removes only the matching orch-owned runtime object.

Names for the CNI network, bridge, and nftables table are deterministic hashes
of the verified cluster and node identities. The nftables table contains only:

- same-bridge workload traffic;
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

## Workload DNS

The agent binds authoritative UDP and TCP DNS on port 53 of the bridge gateway.
Every known service has one exact name:

~~~text
service.application.svc.internal.
~~~

Responses contain sorted private IPv4 A records for serving allocations with a
five-second TTL. AAAA returns NODATA. A known service with no serving allocation
returns NODATA; an unknown name below `svc.internal.` returns NXDOMAIN.
Non-private names use a bounded set of host resolvers after removing the orch
gateway and forwarding loops. Queries, payloads, and response bodies are not
persisted or logged by default.

Each allocation receives a read-only generated `resolv.conf` pointing to the
gateway and using the application service zone as its search suffix.

## Readiness, updates, and DNS drain

Process phase and health are separate. For an HTTP service with `healthPath`,
the agent sends a direct GET to the allocation IP and declared port, sets Host
to `service.application.svc.internal`, rejects redirects, and accepts status
200 through 399. An HTTP service without a path and without public domains uses
a TCP connect. Attempts time out after one second and run every two seconds.
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

## Caddy and public routes

Caddy receives deterministic native JSON containing exact validated domains
and READY private upstreams. Candidate configuration is validated before the
permissioned Unix admin API atomically loads it. The agent probes every HTTP
route and every HTTPS route with its configured SNI before accepting the new
fingerprint. Validation, load, or probe failure retains the last accepted
configuration.

`https = true` is the default and uses Caddy automatic certificate management
plus an HTTP-to-HTTPS redirect. `https = false` creates an explicit port-80
route. Wildcards, IP literals, single-label names, embedded ports,
`svc.internal`, `localhost`, non-ASCII text, and non-canonical domains are
rejected. Cross-application conflicts are deterministic: the earliest created
application owns the exact domain, and a later claimant is `BLOCKED` with
`domain_conflict`.

For public HTTPS, the operator must:

- point every domain's public A record at the node;
- allow inbound TCP 80 and 443 through host and provider firewalls; and
- preserve the ingress data directory so ACME account and certificate state
  survive restarts.

orch does not edit public DNS or cloud firewalls. Caddy operational logs are
protected and bounded; request/access bodies are disabled by default. The
admin API is a protected Unix socket, never a TCP listener.

## Raw TCP routes

`tcp = new Tcp { port = 9418; publicPort = 9418 }` binds the declared IPv4 host
port and forwards unmodified streams to READY private replicas in round-robin
order. Port 53 is reserved for workload DNS and 80/443 for Caddy. Conflicts are
resolved globally by earliest application creation time; a later application
reports `tcp_port_conflict`. Removing or replacing a route stops new accepts
and gives existing connections 30 seconds to drain.

Raw TCP ingress does not open operating-system or cloud-provider firewalls.
There is no virtual IP or cross-node routing mesh in the single-node release.

Start the agent with explicit paths and identity:

~~~console
orch agent serve \
  --data-dir /var/lib/orch-agent \
  --containerd-socket /run/containerd/containerd.sock \
  --snapshotter overlayfs \
  --cni-bin-dir /opt/cni/bin \
  --caddy-binary /usr/bin/caddy \
  --ingress-user orch-ingress \
  --ingress-data-dir /var/lib/orch-ingress
~~~

## Status and stable failures

`orch nodes` reports the node subnet, CNI spec and plugin versions, nftables
capability, DNS capability/state, Caddy version, ingress state, and draining
object count. It does not disclose configured filesystem paths.

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
`network_cidr_conflict`, `caddy_unavailable`, and `domain_conflict`. Inspect the
node and application reason before changing host state.

Meaningful transitions emit deduplicated events such as `network.ready`,
`allocation.ready`, `allocation.unhealthy`, `route.active`, `route.failed`,
`application.healthy`, and `application.rolled_back`. Probe successes and
heartbeats do not create event noise.

## Outages, restart, removal, and backups

During controller or controller-network loss, the agent continues its cached
assignment, probes, DNS, Caddy, and running tasks. It cannot accept new desired
state. On agent restart it reconciles the durable assignment, serving snapshot,
CNI attachment records, containerd ownership labels, and the verified Caddy
process identity; it never adopts an unknown PID, socket, lease, or task.

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

Node-local CNI state is recovery metadata for that node, not a portable cluster
backup. Restoring it onto a different live host or alongside a running agent is
unsupported.

## Verification boundary

Portable unit, integration, race, generation, and cross-build checks run in
native change.codes CI. On a disposable compatible Linux host, run:

~~~console
mise install
mise exec -- make containerd-e2e
mise exec -- make network-e2e
~~~

These opt-in commands require host authority and may create real network
namespaces, a uniquely owned nftables table plus a foreign-preservation
sentinel, containerd tasks, DNS listeners, and Caddy listeners. `network-e2e`
starts the Pebble 2.10.1 tool pinned by
`mise.toml` on ephemeral loopback ports, uses an ephemeral trust root, and
proves trusted automatic HTTPS, redirect, and certificate reuse without
contacting a public CA. The commands are not silently replaced by fake success
on an unprivileged runner. The exact privileged and isolated-ACME results must
be attached as change.codes evidence for the revision being landed.
