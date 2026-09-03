# Product boundary

## Promise

orch should let a developer move an ordinary OCI application from a laptop to
a small group of Linux servers without first becoming a platform engineer.
The experience is one application file, one CLI, and one tested node bundle.

The product is successful when it provides cluster-level recovery with
Compose-level conceptual weight. It is not successful merely because it has a
scheduler.

## Primary user

A small engineering team operating 1–20 mutually reachable cloud VMs or
bare-metal hosts. The team owns its DNS, can open ports between nodes, deploys
fewer than roughly 100 applications, and does not need multi-tenant platform
governance.

## Core journeys

### First HTTPS deployment

1. Initialize a controller.
2. Join one or more workers with an expiring, single-use token.
3. Declare services, resources, routes, and secret references in a typed,
   composable `orch.pkl` module.
4. Run orch plan and understand the intended change.
5. Run orch deploy and receive a healthy HTTPS endpoint.

The target for three clean hosts is under 15 minutes without installing an
external database, discovery service, secret manager, or ingress controller.

### Safe update and rollback

1. Change an image tag or application setting.
2. Inspect an immutable, semantic plan.
3. Deploy while health gates and availability budgets protect traffic.
4. Automatically restore the last healthy revision if readiness fails.
5. Explicitly roll back to any previous revision without rewriting history.

### Failed-node recovery

1. See a node transition from healthy to unavailable with a concrete reason.
2. Replace stateless allocations on healthy nodes.
3. Keep a local-volume allocation blocked on its volume node rather than
   pretending empty storage is a recovery.
4. Keep existing traffic and agent-local restarts working while the controller
   itself is unavailable.

## Product rules

- The public abstraction is an application made of services.
- Configuration is strict, versioned, and statically inspectable.
- Every state transition must explain what is happening and why.
- The controller distributes intent but never proxies application traffic.
- Local state is never presented as replicated state.
- Secure node identity and encrypted cross-node traffic are defaults.
- A new feature must serve one of the core journeys or wait.

## Non-goals for v0.1

- Kubernetes compatibility, Helm, CRDs, operators, or a generic controller API.
- Docker Engine or Docker Swarm compatibility.
- Multi-region operation, multi-tenant RBAC, autoscaling, or preemption.
- Distributed storage, automatic database failover, or volume replication.
- A dashboard, hosted service, build platform, or mandatory GitOps controller.
- NAT traversal, Windows, rootless nodes, IPv6 overlay networking, service
  meshes, and public non-HTTP protocols.

## Validation gates

Before the distributed runtime contract is frozen:

- The two-service example must stay below 60 non-comment lines.
- Three target users must complete the core journeys from the documentation
  without needing scheduler terminology.
- The same reference application must be compared empirically with Compose,
  Swarm, k3s, and Nomad.
- Work stops for a product rethink if the integrated route, certificate,
  secret, and rollback workflow is not materially simpler.

Human usability sessions and timed competitor runs require real participants
and clean hosts. They remain explicit product gates and are not claimed by the
configuration foundation.

## Current implementation boundary

The current repository implements the one-node vertical slice of these
journeys: containerd execution, a private CNI subnet, readiness-gated local
DNS, exact-domain Caddy ingress with automatic HTTPS, encrypted application
secrets, and authenticated exact-host registry pulls. The controller stays out
of the data path, and the agent keeps its last accepted serving state while the
controller is unavailable. Multi-node placement, WireGuard routing,
rescheduling, and release packaging remain later slices. See
[networking.md](networking.md) and [runtime.md](runtime.md) for the precise
operator contract rather than inferring future guarantees from the product
promise.
