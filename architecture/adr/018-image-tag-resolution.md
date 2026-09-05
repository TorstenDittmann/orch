# 018: Resolve image tags at deployment acceptance

## Decision

Resolve service and cron job tags at the controller before persisting desired
state. Store the resolved canonical manifest in the existing immutable revision
and the authored canonical manifest in a new immutable `requested_manifest`
column. Existing revisions fall back to their stored canonical manifest.

Apply idempotency hashes the authored request and returns its original digest
in the acceptance receipt. Application and deployment digests hash resolved
intent. Check replay before registry access and again inside the transaction:
concurrent requests may observe different tag values, but the first committed
resolution and response win. A no-op requires both requested and resolved intent
to match. A fresh key re-resolves tags; an accepted key never does.

Rollback copies both manifests and uses its existing generation guard and
operation-specific replay hash. It never contacts the registry. Worker caches
or registry retention are still needed to obtain the recorded image layers.

Use the pinned containerd distribution client over HTTPS with exact-host stored
credentials, bounded calls, verified metadata digests, and sanitized errors.
Suppress upstream resolver logging because registry messages and token URLs
are not safe operator diagnostics. Do not persist credentials in either
manifest. Keep multi-platform index digests; validate Linux amd64/arm64 support
without selecting the controller platform. Workers retain final platform
selection and digest-only validation.

## Consequences

New tag deployments need controller-to-registry connectivity. Resolution errors
reject the complete request, leaving desired state untouched. Explicit digests
keep their existing offline acceptance behavior. Old unresolved tag revisions
are not silently rewritten and require a fresh deploy. Receipt and resolved
revision digests intentionally differ for tags and are documented separately.
The migration is forward-only and adds no binary downgrade guarantee.

## Verification

Local TLS registry fixtures exercise public/private resolution, image and index
digests, supported platforms and error classification. Controller/store tests
cover tag movement, same-request caching, concurrent acceptance, durable replay,
rollback and credential redaction. CLI/TUI checks cover requested and resolved
image display. Native change.codes CI runs the repository checks.
