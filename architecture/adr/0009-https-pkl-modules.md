# ADR 0009: permit HTTPS Pkl modules

Status: Accepted.

## Decision

Operator-side Pkl evaluation permits `https:` modules in addition to rooted
local modules, the Pkl standard library, and the embedded `orch:` schema.
Plaintext HTTP modules, package schemes, external readers, and every resource
scheme remain denied. The same policy applies to `deploy`, `validate`, `plan`,
and `render`.

Production configurations should pin HTTPS imports to immutable commits or
releases. The local evaluation root does not constrain HTTPS destinations.

## Consequences

- Editors and stock Pkl tooling can resolve schemas and shared modules directly
  from ordinary HTTPS endpoints without understanding the `orch:` reader.
- Evaluation now depends on operator-side network availability, DNS, TLS, and
  the availability of every remote module.
- Imported modules are trusted executable configuration and can make requests
  to HTTPS services reachable from the operator machine. Configuration authors
  must be trusted accordingly.
- Mutable URLs can resolve to different desired state between commands or
  retries. The manifest digest covers resolved canonical JSON, not source bytes,
  URLs, redirects, or the import graph.
- Pkl source still never crosses the apply API, and controllers and agents still
  do not execute Pkl or make module requests.
