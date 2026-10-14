# ADR 024: Public WireGuard endpoints

## Status

Accepted. Amends ADR 0013.

## Context

ADR 0013 required every node's advertise address, and therefore every
WireGuard peer endpoint, to be a private IPv4 address. Machines rented from
different providers or regions often share no private network, and overlay VPNs
such as Tailscale use 100.64.0.0/10, which Go does not classify as private. Such
clusters could not form a mesh at all.

## Decision

A node's advertise address may be any canonical unicast IPv4 address: private,
shared address space, or public. The controller distributes it unchanged as the
node's WireGuard peer endpoint. Unspecified, `0.0.0.0/8`, broadcast, loopback,
multicast, link-local, and IPv6 addresses remain rejected by the agent, the
controller's node registry, assignment validation, and `orch node install`.

Only the underlay changes. The workload CIDR, node subnets, allocation
addresses, cluster DNS answers, health targets, and ingress upstreams remain
RFC1918 and continue to be validated as such. WireGuard already authenticates
every peer by public key and encrypts all traffic, so a public endpoint exposes
no workload traffic. It does expose the WireGuard UDP port to the internet;
orch still leaves host and provider firewalls to the operator, and `orch node
doctor` warns when the advertise address is public that UDP 51820 should be
allowed only from the other nodes' addresses.

orch still provides no NAT traversal, relay, or endpoint discovery. A node must
own its advertise address locally, and every node must reach every other
node's advertise address directly.

## Consequences

- Clusters can span providers and regions without a private network or VPN.
- Cross-region traffic pays the real network latency, for example about 90 ms
  between Frankfurt and US East. Placement constraints remain the way to keep
  chatty services in one region.
- A host whose public address is only provided through 1:1 NAT and not
  assigned to an interface still cannot advertise it. A separate WireGuard
  endpoint override can be added later if that becomes necessary.
- Existing clusters are unaffected: every previously valid identity remains
  valid and no stored data changes.

## Rejected alternatives

- A separate `--wireguard-endpoint` flag next to a private advertise address
  would add a second address with no other consumer, since the advertise
  address is used only as the WireGuard endpoint.
- Relays or NAT traversal would add a data-plane component that ADR 0013
  deliberately excludes.
