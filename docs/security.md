# Control-plane security model

The current control plane establishes local identity, secure file boundaries,
and an authenticated HTTPS API for one controller. It does not yet provide
certificate rotation, revocation, worker enrollment, additional administrator
issuance, or multi-user RBAC.

## Identity hierarchy

Initialization creates a P-256 private cluster CA and two leaf identities:

- The controller certificate has server-auth and client-auth extended key
  usages. Its DNS name or IP SAN is taken from `--server`.
- The administrator certificate has client-auth usage only.
- Each leaf has a cluster- and role-specific SPIFFE URI of the form
  `spiffe://orch/cluster/<cluster-id>/<role>`.

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

The metadata-free `/healthz` endpoint is the only route that permits a TLS
connection without a client certificate. It reports no version, cluster,
application, or event information.

## Secret placement

The CA private key and controller private key stay under the controller data
directory. The administrator private key stays beside its local context under
the cluster-specific identity directory. Only the CA certificate is copied to
both locations.

Private directories use mode `0700`. The database, context, certificates, and
private keys use mode `0600`; existing identity material that grants group or
world access is rejected. Symlinks are not accepted where a private file or
identity directory is expected.

Private keys and PEM certificates are not written to API responses, CLI output,
events, or the SQLite database. Initialization JSON contains identifiers and
filesystem paths only; routine remote JSON does not disclose credential paths.
The context stores relative paths rather than embedding key material.

Application events contain only the application name, generation, revision ID,
digest, and `PENDING` status. Canonical desired state is returned only by the
authenticated application read API. Manifest secret fields hold references,
not values; secret-value storage is not implemented.

## Operator responsibilities

- Restrict operating-system access to the controller account and
  administrator context.
- Back up controller PKI and administrator credentials separately from the
  verified database backup, using encryption and access controls appropriate
  for private keys.
- Treat possession of `ca-key.pem` as full cluster identity authority and
  possession of `admin-key.pem` as administrator access.
- Do not expose the data or context directories through application mounts.
- Use a stable HTTPS name or IP for `--server`; initialization intentionally
  will not rotate the controller certificate when that endpoint changes.
- Treat idempotency keys as non-secret request identifiers. Preserve a key with
  the exact manifest when retrying an uncertain deployment request.

The current single-node design stores the CA key online because later worker
enrollment will need signing. Moving signing offline or introducing an
intermediate CA requires a separate design and migration plan.
