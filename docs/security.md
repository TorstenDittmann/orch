# Controller and agent security model

The current slice establishes local identity, secure file boundaries, an
authenticated HTTPS API for one controller, and administrator-authorized
creation of join credentials for at most 20 active agents. It automatically
renews agent leaf certificates, but does not provide CA, controller,
administrator, or agent-key rotation, revocation lists, additional administrator
issuance, or multi-user RBAC.

## Identity hierarchy

Guided controller installation writes two explicit private handoff directories.
The administrator directory contains a client identity with full controller
authority. The recovery set contains the cluster CA private key, controller
identity, secret master key, and a database snapshot. Both must be transferred
over a protected channel, stored off-host with mode-equivalent access controls,
and excluded from source control, logs, and ordinary artifacts. The recovery
snapshot is created at installation time and does not replace ongoing backups.

Managed upgrades accept only a local regular candidate executable with safe
ownership and modes. The executable must report a compatible machine-readable
build descriptor before any service is stopped. Controller upgrades create a
fresh private recovery set; worker upgrades copy private agent state while the
agent is stopped. Journals contain paths, phases, descriptors, and hashes but no
certificate, key, registry credential, secret value, or environment contents.

Automatic rollback ends at authenticated readiness. Beyond that point, an old
controller database or agent assignment cache may be stale, so orch reports the
recovery path instead of silently discarding accepted state.

Initialization creates a P-256 private cluster CA and two leaf identities.
Each agent enrollment later signs another leaf identity:

- The controller certificate has server-auth and client-auth extended key
  usages. Its DNS name or IP SAN is taken from `--server`.
- The administrator certificate has client-auth usage only.
- The agent generates its own P-256 private key and signed CSR. Its certificate
  has client-auth usage only and the exact URI
  `spiffe://orch/cluster/<cluster-id>/agent/<node-id>`.
- Each leaf has a cluster- and role-specific SPIFFE URI of the form
  `spiffe://orch/cluster/<cluster-id>/<role>`; the agent URI adds its node ID.

The CA is valid for ten years and leaf identities for one year, with a small
clock-skew allowance. Agent leaf certificates renew automatically with the same
private key and stable node identity. Other expired or damaged identity material
is never silently replaced.

## Transport and authorization

The controller requires TLS 1.3. Its certificate must chain to the cluster CA,
match the advertised server hostname or IP, carry server-auth and client-auth
usages, and contain exactly the controller URI for the durable cluster ID.

Operator methods require a client certificate that:

- chains to this controller's cluster CA;
- is valid for client authentication;
- is not a CA certificate; and
- contains exactly one identity URI,
  `spiffe://orch/cluster/<cluster-id>/administrator`.

No certificate, a controller-role certificate, a foreign cluster certificate,
the wrong cluster URI, duplicate identity URIs, an unexpected extended key
usage, or an expired certificate is rejected. The CLI performs the symmetric
controller role/cluster check in addition to normal TLS hostname and chain
verification.

Remembered workstation targets copy the validated administrator context and
credentials into a versioned private directory under the user configuration
directory. Target directories use mode `0700` and files mode `0600`; imports
reject symlinks, unsafe names, unexpected fields, and overwrite. Target listing
and diagnostics never print key material or credential paths.

A target route cannot relax controller authentication. Direct transport uses
the canonical URL. WireGuard transport substitutes only a `host:port` dial
address reachable through an interface already configured by the operating
system, while TLS still verifies the canonical hostname. SSH transport starts
the system OpenSSH client with argv (never a shell), an option-safe host alias,
batch authentication, forwarding-failure detection, and a loopback-only local
forward. OpenSSH retains responsibility for host keys, agents, keys, and
ProxyJump. The forwarded connection still performs controller TLS validation
and administrator mTLS; no credential is read from the SSH host.

The agent stream is a separate ConnectRPC route. It requires the exact active
node's enrolled certificate, public-key fingerprint, certificate serial,
cluster ID, and node ID. An administrator or controller certificate cannot
open it. One healthy stream is permitted per node identity; concurrent streams
from different active nodes are independent. A removed node certificate is
rejected immediately.

An administrator registers an expiring join ID, exact node name, and SHA-256
hash of a locally generated 256-bit secret. The controller never receives or
stores the plaintext secret until it is presented for enrollment. Join list and
event records contain metadata only. A join is single-use, may be revoked before
use, expires at its declared instant, and binds exact replay to the same token,
node, CSR, and idempotency key.

`EnrollmentService.EnrollAgent` is the only ConnectRPC method that permits a TLS
connection without a client certificate. Its client pins the CA embedded in the
join artifact and still verifies the advertised hostname, controller role, and
cluster ID. All invalid, expired, revoked, unknown, or conflicting credentials
share one public authentication error. The agent private key never crosses the
network: `agent init` durably stages it, its CSR, and its retry key before sending
the request. The controller ignores requested CSR SANs and signs only the
computed node identity.

The metadata-free `/healthz` endpoint is the only other route without a client
certificate. It reports no version, cluster, application, or event information.
The deprecated administrator-authenticated enrollment RPC remains available
only for wire compatibility; the official CLI never uses it for fresh nodes.

Agent renewal is authenticated by the exact current certificate and accepts a
CSR only when its public key matches the enrolled key. Issuance creates one
pending credential without retiring the current one. Either credential may
authenticate the reconnect, but the pending credential cannot request another
renewal. A successful Hello atomically promotes it; later connections using the
old serial fail. Renewal events contain the node and expiry, never key or token
material.

If renewal does not finish before expiry, an already-running agent continues
cached local supervision but cannot reauthenticate after a disconnect or
restart. Lost/expired identity recovery and private-key replacement are outside
this release. Removal remains the immediate node revocation mechanism and is not
proof of host fencing: isolate an unreachable node before using
`--lost --confirm-fenced`.

## Configuration evaluation

Pkl executes only on the operator machine for `deploy`, `validate`, `plan`, and
`render`. The evaluator accepts the pinned Pkl version, a single bounded entry
module, local module imports below a real-path root, standard-library modules,
HTTPS modules, and the embedded versioned `orch:` schema. Its ten-second
deadline follows CLI cancellation.

The evaluator denies every resource scheme, including environment variables,
external properties, local files, HTTP(S), and packages. It also denies
plaintext HTTP modules, package, project-package, module-path, REPL, and
unregistered module schemes. It does not load user settings or `PklProject`
evaluator settings. Root and symlink boundary tests, HTTPS evaluation tests,
and a loopback request sentinel enforce that policy.

HTTPS imports make outbound requests from the operator machine and are not
constrained by the local evaluation root. Operators must treat imported modules
as executable configuration, use trusted endpoints, and pin immutable URLs.
Arbitrary HTTPS destinations can reach private services visible to the operator
and mutable content can change between commands or retries. TLS authenticates
transport; the resolved manifest digest does not authenticate the import graph
or remote source bytes.

Pkl source and imports never cross the API. The authenticated apply request
contains only deterministic canonical JSON. The controller strictly decodes
that JSON, rejects unknown/trailing/noncanonical data, reruns Go validation,
and stores the same canonical contract used by agents. Controllers and agents
start and reconcile without Pkl present on `PATH`.

## Secret placement

The CA private key and controller private key stay under the controller data
directory. The administrator private key stays beside its local context under
the cluster-specific identity directory. The agent private key stays under its
own `pki` directory. Only the CA certificate and signed public certificates are
copied between roles.

Private directories use mode `0700`. The database, context, certificates, and
private keys use mode `0600`; existing identity material that grants group or
world access is rejected. Symlinks are not accepted where a private file or
identity directory is expected.

Private keys are not written to API responses, CLI output, events, or either
SQLite database. Enrollment returns only the CA and signed agent certificate.
Initialization JSON contains identifiers and filesystem paths only; routine
remote JSON does not disclose credential paths. The context stores relative
paths rather than embedding key material.

Application events contain desired-state identifiers or status reason codes,
not workload logs, exec payloads, or secret values. Canonical desired state is
returned only by the authenticated application read API. Manifest secret
fields hold references, not values. Values and registry credentials are
immutable AES-256-GCM envelope-encrypted versions; metadata list APIs never
return plaintext.

The controller key-encryption key and the agent cache key are separate mode
`0600` files outside their SQLite databases. Assignment records contain only
version references. Resolved values travel in a separate transient bundle over
the mutually authenticated agent stream, and the agent encrypts that bundle
under its cache key before acknowledging the assignment. A missing or
mismatched key fails closed.

Each multi-node agent also owns a distinct mode-`0600` WireGuard private key
outside SQLite. Only its public key is reported to the controller and assigned
to peers. The controller authorizes an exact public key, private IPv4 endpoint,
UDP port, and `/24` per active peer; an agent rejects catalogs containing
unknown subnets or keys. Cross-node workload traffic is encrypted by
WireGuard, but encryption does not supply workload identity or network policy.

The controller never persists workload log bytes, exec arguments, or exec
output. The agent keeps stdout and stderr in separate protected, bounded files.
Exec is non-interactive, deadline-bound, and routed only to an exact running
allocation.

## Workload isolation boundary

containerd namespaces organize orch resources but are not a security boundary.
The agent additionally requires deterministic IDs and exact cluster, node,
allocation, and spec-digest labels before it adopts, stops, or removes an
object. Label mismatch fails closed.

Workloads receive fresh PID, mount, IPC, UTS, cgroup, and network namespaces,
no Linux capabilities, no-new-privileges, no host devices, and explicit cgroups
v2 CPU/memory limits. Only declared agent-managed local volumes and read-only
secret projections are bind mounted. Secret projection sources live on a
root-only, no-exec tmpfs; secret environment variables are loaded by an init
handoff rather than stored in OCI metadata. The containerd socket, controller
state, agent identity, and agent state are never mounted. The agent attaches
each task to its own network namespace with the pinned CNI plugins. It owns only
a deterministic bridge, host-local allocation directory, application resolver
files, and uniquely named nftables table; a missing ownership marker or an
overlapping host route fails closed.

Caddy is a separate process under a dedicated non-root account. Its pinned,
root-owned binary receives only `cap_net_bind_service`; the agent fixes its
primary and supplementary groups and supplies only a minimal HOME/XDG/PATH
environment. The agent validates the binary, process UID, PID/executable
identity, private data directory, and Unix admin socket before adoption. Caddy
receives ready private upstream addresses, not controller or containerd
credentials.

## Operator responsibilities

- Restrict operating-system access to the controller account and
  administrator targets. Treat a copied target as administrator access.
- Back up controller PKI and administrator credentials separately from the
  verified database backup, using encryption and access controls appropriate
  for private keys.
- When encrypted records exist, supply `--secret-key-output` to controller
  backup and store that key separately from the encrypted database snapshot.
- Treat possession of `ca-key.pem` as full cluster identity authority and
  possession of `admin-key.pem` as administrator access. Treat possession of
  `agent-key.pem` as control of the enrolled node identity.
- Do not expose the data or context directories through application mounts.
- Use a stable HTTPS name or IP for `--server`; initialization intentionally
  will not rotate the controller certificate when that endpoint changes.
- Treat idempotency keys as non-secret request identifiers. Preserve a key with
  the exact manifest when retrying an uncertain deployment request.
- Restrict access to the containerd socket. The agent requires substantial host
  authority even though workload OCI specs are constrained.
- Install only the documented CNI and Caddy versions, keep their binaries
  root-owned and non-writable, and do not reuse the orch nftables ownership
  marker or Caddy account for unrelated services.
- Back up local volume directories separately. They are preserved on remove
  but are neither replicated nor present in controller backups.
- Permit WireGuard UDP (51820 by default) only between the canonical private
  node addresses and keep those addresses mutually reachable. orch does not
  provide NAT traversal, a relay, or automatic cloud firewall changes.
- Treat `--lost --confirm-fenced` as a safety assertion, not a cleanup shortcut.
  Verify the old host cannot run its cached assignment before permitting cron
  ownership or a node subnet to be reused.

The single-controller design stores the CA key online for agent enrollment.
Moving signing offline or introducing an intermediate CA requires a separate
design and migration plan.

## Partition and controller-outage boundary

The controller removes an unreachable node's endpoints from new discovery and
ingress catalogs and may replace its stateless service attempts after the
60-second loss grace. It cannot revoke an assignment already cached on an
isolated host. That host may continue running processes and restarting them,
so a partition can temporarily create duplicate stateless instances. orch
does not claim fencing or strong singleton execution.

Local-volume allocations never move automatically. Cron ownership moves only
after graceful acknowledgement or explicit confirmed fencing. During a
controller outage, agents continue their last authenticated assignment,
WireGuard peer set, DNS, ingress, cron schedules, and local restart behavior;
no new placement or rollout decision is authorized.
