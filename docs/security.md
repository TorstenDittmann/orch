# Controller and agent security model

The current slice establishes local identity, secure file boundaries, an
authenticated HTTPS API for one controller, and administrator-authorized
enrollment for exactly one agent. It does not yet provide certificate rotation,
revocation, node replacement, additional administrator issuance, or multi-user
RBAC.

## Identity hierarchy

Initialization creates a P-256 private cluster CA and two leaf identities.
Agent enrollment later signs a third leaf identity:

- The controller certificate has server-auth and client-auth extended key
  usages. Its DNS name or IP SAN is taken from `--server`.
- The administrator certificate has client-auth usage only.
- The agent generates its own P-256 private key and signed CSR. Its certificate
  has client-auth usage only and the exact URI
  `spiffe://orch/cluster/<cluster-id>/agent/<node-id>`.
- Each leaf has a cluster- and role-specific SPIFFE URI of the form
  `spiffe://orch/cluster/<cluster-id>/<role>`; the agent URI adds its node ID.

The CA is valid for ten years and the leaf identities for one year, with a
small clock-skew allowance. These lifetimes are an initial bootstrap contract,
not a rotation policy. Expired or damaged material causes repeated
initialization to fail; it is never silently replaced.

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

The agent stream is a separate ConnectRPC route. It requires the exact enrolled
agent certificate, public-key fingerprint, certificate serial, cluster ID, and
node ID. An administrator or controller certificate cannot open it. A healthy
stream prevents another process holding copied agent credentials from taking
over; a stale session is replaceable only after the heartbeat timeout.

Enrollment itself is an administrator-only API. The agent private key never
crosses the network: `agent init` durably stages the private key, CSR, and
idempotency key before sending the CSR. The controller ignores requested CSR
SANs and signs only the computed node identity. Lost or expired agent identity
cannot be silently replaced in this release.

The metadata-free `/healthz` endpoint is the only route that permits a TLS
connection without a client certificate. It reports no version, cluster,
application, or event information.

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
not workload logs or exec payloads. Canonical desired state is returned only by
the authenticated application read API. Manifest secret fields hold references,
not values; any such application is `BLOCKED` because secret-value storage is
not implemented.

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
v2 CPU/memory limits. Only declared agent-managed local volumes are bind
mounted. The containerd socket, controller state, agent identity, and agent
state are never mounted. The agent attaches each task to its own network
namespace with the pinned CNI plugins. It owns only a deterministic bridge,
host-local allocation directory, application resolver files, and uniquely named nftables
table; a missing ownership marker or an overlapping host route fails closed.

Caddy is a separate process under a dedicated non-root account. Its pinned,
root-owned binary receives only `cap_net_bind_service`; the agent fixes its
primary and supplementary groups and supplies only a minimal HOME/XDG/PATH
environment. The agent validates the binary, process UID, PID/executable
identity, private data directory, and Unix admin socket before adoption. Caddy
receives ready private upstream addresses, not controller or containerd
credentials.

## Operator responsibilities

- Restrict operating-system access to the controller account and
  administrator context.
- Back up controller PKI and administrator credentials separately from the
  verified database backup, using encryption and access controls appropriate
  for private keys.
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

The current single-node design stores the CA key online for agent enrollment.
Moving signing offline or introducing an intermediate CA requires a separate
design and migration plan.
