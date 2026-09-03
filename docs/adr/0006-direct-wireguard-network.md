# ADR 0006: direct WireGuard routes and DNS endpoints

Status: Accepted by Plan #1. Single-node foundation implemented by Plan #5;
multi-node peer routing implemented by Plan #9.

Implementation note: the first node owns a deterministic `/24`, CNI bridge,
host-local addresses, nftables forwarding, and health-filtered local DNS. Plan
#9 retains that first subnet while adding peers, cross-node routes, and a
global healthy-endpoint catalog. ADR 0013 defines placement and failure safety.

Each node receives a workload subnet. A managed WireGuard full mesh routes
directly between those subnets, while local DNS returns healthy allocation
addresses. Public traffic enters only through labeled ingress nodes.

There is no every-node routing mesh or virtual service IP. The full mesh is
bounded by the stated 20-node ceiling. Nodes must already have mutually
reachable private addresses; NAT traversal is excluded.
