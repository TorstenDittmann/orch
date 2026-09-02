# Bootstrap security model

The bootstrap layer establishes local identity and secure file boundaries for
one future controller. It does not yet provide certificate rotation,
revocation, worker enrollment, remote authorization, or transport listeners.

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

## Secret placement

The CA private key and controller private key stay under the controller data
directory. The administrator private key stays beside its local context under
the cluster-specific identity directory. Only the CA certificate is copied to
both locations.

Private directories use mode `0700`. The database, context, certificates, and
private keys use mode `0600`; existing identity material that grants group or
world access is rejected. Symlinks are not accepted where a private file or
identity directory is expected.

Private keys and PEM certificates are not written to CLI output, events, or
the SQLite database. JSON output contains identifiers and filesystem paths
only. The context stores relative paths rather than embedding key material.

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

The current single-node design stores the CA key online because later worker
enrollment will need signing. Moving signing offline or introducing an
intermediate CA requires a separate design and migration plan.
