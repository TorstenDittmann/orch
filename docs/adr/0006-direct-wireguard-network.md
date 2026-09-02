# ADR 0006: direct WireGuard routes and DNS endpoints

Status: Accepted by Plan #1.

Each node receives a workload subnet. A managed WireGuard full mesh routes
directly between those subnets, while local DNS returns healthy allocation
addresses. Public traffic enters only through labeled ingress nodes.

There is no every-node routing mesh or virtual service IP. The full mesh is
bounded by the stated 20-node ceiling. Nodes must already have mutually
reachable private addresses; NAT traversal is excluded.
