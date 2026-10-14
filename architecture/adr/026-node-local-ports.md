# ADR 026: Node-local TCP and UDP ports

## Status

Accepted. Amends ADR 0010 and ADR 0013.

## Context

ADR 0010 publishes raw TCP on every ingress node through a user-space proxy.
That works for databases and similar services, but not for game servers, voice
relays, or other workloads that speak UDP, expect clients to connect to the
machine that runs them, or need the client's source address. A proxy hides the
source address, and an ingress-wide UDP proxy would need session tracking that
orch does not want to own.

## Decision

A service may set `tcp.nodePort` and a new `udp { port; nodePort }` endpoint.
Each replica's port is published on the node that runs it, and only there.

- **Data path.** The agent adds DNAT rules to its own nftables table: a
  prerouting NAT chain matches traffic that arrives on a non-bridge interface
  for a local address and the published protocol and port, and rewrites it to
  the allocation's address and target port. A `published` chain, jumped to from
  the forward chain before its drop rule, accepts that traffic. There is no
  user-space proxy and no SNAT, so the container sees the client address.
  Locally generated traffic is not translated.
- **Conntrack.** Whenever a mapping is added, changed, or removed, the agent
  deletes conntrack entries for that protocol and port so existing UDP flows
  follow the new allocation instead of the old address.
- **Readiness.** UDP requires an `http` or `tcp` endpoint on the same service.
  A port is published only after that endpoint's health check passes and is
  withdrawn when the allocation stops serving. UDP is never probed.
- **Scheduling.** Published ports are node resources. The scheduler places at
  most one allocation per protocol and port on a node and reports
  `node_port_in_use` when no node is free. Applications with node ports always
  use the distributed reconciler.
- **Ownership.** As with TCP public ports in ADR 0010, a protocol and port
  belongs to the earliest application that uses it, across the whole cluster.
  Later applications are blocked with `node_port_conflict`. Services in the
  same application may share a port; the scheduler then keeps them on different
  nodes. A TCP node port may not equal an ingress `publicPort` in the same
  application.
- **Reserved ports.** TCP 53, 80, and 443 and UDP 53 and 51820 are rejected by
  configuration and agent validation. Before publishing, the agent also refuses
  a port that a host process is listening on and reports
  `node_port_unavailable`, so a manifest cannot take over SSH or the
  controller's port.
- **Rollouts.** An old replica must release its port before its replacement can
  bind it on the same node, so configuration validation rejects `maxSurge` above
  zero for any application that publishes node ports. Deploy settings stay per
  application.
- **Status.** Agents report each published port with its state, and the
  controller stores reports only when they match an assigned contract. `orch app
  status` and the TUI list them next to TCP routes.

## Consequences

- Clients connect to node addresses, so they need a node's address and that
  node's firewall must allow the port. orch does not move ports between nodes
  or provide a stable shared address.
- Rollouts of node-port services have a short gap per replica.
- A connection from the node to its own address and published port is not
  translated; tests must come from another machine.
- The controller schema advances to 12 for the observations table. Protocol
  changes are additive, so the agent protocol version does not change, but
  agents must be upgraded before applications use `nodePort` or `udp`. An
  older agent ignores the new fields and would run the replica unpublished.

## Rejected alternatives

- A user-space UDP proxy on ingress nodes would lose the client address and
  would need its own session table and timeouts.
- `hostNetwork` style allocations would expose every container port and bypass
  the per-allocation network namespace and firewall.
- Allowing surge with a temporary second port would change the client-facing
  port during rollouts.
