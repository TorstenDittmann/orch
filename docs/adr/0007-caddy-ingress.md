# ADR 0007: supervise Caddy for HTTP ingress

Status: Accepted by Plan #1.

Ingress nodes run a pinned Caddy process configured from validated application
routes and healthy endpoints. Configuration updates are rendered
deterministically, validated, and reloaded atomically.

This gives the product automatic HTTPS and mature proxy behavior without
placing the controller in the data path or implementing a new proxy.
