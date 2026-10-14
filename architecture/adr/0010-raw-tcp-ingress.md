# ADR 0010: raw TCP ingress is a service endpoint

## Status

Accepted; amended by ADR 026.

## Decision

A service may declare exactly one endpoint: `http = new Http { ... }` or
`tcp = new Tcp { ... }`. `Tcp.port` is the container port. An optional
`Tcp.publicPort` binds an IPv4 listener on every ingress-enabled node. There
is no separate ingress object.

The agent forwards bytes without protocol inspection to READY private
allocation addresses using deterministic round-robin selection. A listener is
staged before it becomes active, stops accepting immediately when withdrawn,
and lets established connections drain for 30 seconds.

Public ports range from 1 through 65535; 53, 80, and 443 remain reserved for
managed DNS and HTTP ingress. Across applications, the earliest-created
application owns a conflicting public port. Later applications are `BLOCKED`
with `tcp_port_conflict`.

## Consequences

TCP services share the same readiness, DNS, rollout, and rollback machinery as
HTTP services. Operators remain responsible for host and provider firewall
rules. Each ingress node reaches READY backends over the encrypted node mesh;
the public listener itself remains node-local.
