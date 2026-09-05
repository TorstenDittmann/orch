# ADR 0014: Secure joins and two-phase agent certificate renewal

## Status

Accepted.

## Context

Fresh workers previously needed an administrator context to enroll. Copying an
administrator certificate and key onto every worker made the bootstrap path
more privileged than the resulting agent identity. Agent certificates also had
a one-year lifetime with no renewal path, so an otherwise healthy node would
eventually become unable to reconnect.

Bootstrap must tolerate an uncertain response without allowing a bearer token
to create two identities. Renewal must tolerate a crash before or after local
certificate publication without retiring the worker's only usable credential.

## Decision

The operator CLI generates a random 256-bit secret and versioned join artifact
locally. The artifact binds one node name, cluster ID, controller HTTPS origin,
cluster CA, and expiry. The controller stores only the secret's SHA-256 hash and
bounded metadata. A dedicated certificate-free enrollment service validates the
token before CSR work and commits token consumption, node enrollment, frozen
response, idempotency state, and events in one transaction. Exact replay returns
the frozen response; any changed request is rejected.

The bootstrap client trusts only the artifact CA and verifies the endpoint's
hostname, controller role, and cluster identity. Other control and agent methods
retain mutual-TLS authentication.

Agents renew inside the final 90 days with deterministic jitter. They reuse the
existing P-256 key and node ID. The controller persists one pending certificate
while continuing to accept the current certificate. The agent durably stages
the CSR and retry key, validates and atomically publishes the response, then
reconnects. Authentication of the pending serial followed by a valid Hello
promotes it transactionally and retires the old serial.

## Consequences

- Workers never receive administrator credentials for normal enrollment.
- Join files are short-lived bearer credentials and require private storage,
  protected transfer, and revocation after suspected disclosure.
- Lost bootstrap and renewal responses are safely replayable.
- Node identity, private key, subnet, placements, cron ownership, and local
  volume claims remain stable across renewal.
- Reusing the key does not reduce exposure after key compromise. Private-key,
  CA, controller, and administrator rotation require separate designs.
- An agent that reaches certificate expiry cannot reconnect automatically. Its
  already-running cached workloads continue, but identity recovery is outside
  this decision.
