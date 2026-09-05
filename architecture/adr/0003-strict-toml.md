# ADR 0003: use strict, versioned TOML

Status: Superseded by ADR 0008 and Plan #6.

Applications use one TOML document with an explicit schema version. The parser
rejects unknown fields, resolves visible defaults, emits a deterministic
canonical form, and ships JSON Schema for editors.

YAML was rejected because the product does not need its larger syntax surface.
A programming-language configuration was rejected because static validation,
reviewable diffs, and cross-language tooling matter more than templating.
