# ADR 0007: supervise Caddy for HTTP ingress

Status: Accepted by Plan #1. Implemented for the single-node slice by Plan #5.

Implementation note: the agent validates and supervises pinned Caddy 2.11.3
under a dedicated non-root identity, uses a protected Unix admin socket, and
retains the last accepted native JSON configuration and certificate storage.
Ingress-node selection remains deferred because this slice has one node.

Ingress nodes run a pinned Caddy process configured from validated application
routes and healthy endpoints. Configuration updates are rendered
deterministically, validated, and reloaded atomically.

This gives the product automatic HTTPS and mature proxy behavior without
placing the controller in the data path or implementing a new proxy.
