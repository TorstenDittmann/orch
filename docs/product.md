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
6. Operate the cluster locally through remembered targets and a discoverable,
   keyboard-first TUI without repeatedly locating credential files.

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

Recovery is intentionally bounded. After 60 seconds unreachable, orch may
place new attempts for stateless service replicas and withdraw the old node's
endpoints. A partition can leave the old cached processes alive, so temporary
duplicates are possible. Local-volume services and cron ownership do not move
until their different safety requirements are satisfied.

## Product rules

- The public abstraction is an application made of services.
- Configuration is strict, versioned, and statically inspectable.
- Every state transition must explain what is happening and why.
- The controller distributes intent but never proxies application traffic.
- Local state is never presented as replicated state.
- Secure node identity and encrypted cross-node traffic are defaults.
- Hard label and capacity requirements are never weakened to make a placement.
- A new feature must serve one of the core journeys or wait.

## Non-goals for v0.1

- Kubernetes compatibility, Helm, CRDs, operators, or a generic controller API.
- Docker Engine or Docker Swarm compatibility.
- Multi-region operation, multi-tenant RBAC, autoscaling, or preemption.
- Distributed storage, automatic database failover, or volume replication.
- A browser dashboard, hosted service, build platform, or mandatory GitOps
  controller. The built-in local terminal UI is part of the single CLI.
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

The repository implements these journeys for one through 20 nodes: containerd
execution, private per-node CNI subnets, readiness-gated global DNS,
exact-domain Caddy ingress with automatic HTTPS, raw TCP ingress, encrypted
application secrets, authenticated exact-host registry pulls, deterministic
placement, direct WireGuard routing, secure single-use worker joins, automatic
same-key agent-certificate renewal, remembered direct/WireGuard/SSH operator
targets, a built-in terminal dashboard, node operations, and guided persistent
controller and worker installation on Ubuntu and Fedora. This remains a
single-controller design: during a controller outage
agents preserve their last accepted data plane, but deployments, scheduling,
drain, and rollout progress pause. See
[networking.md](networking.md) and [runtime.md](runtime.md) for the precise
operator contract rather than inferring future guarantees from the product
promise.

The distributed contract deliberately does not promise automatic fencing,
singleton execution during partitions, exactly-once cron, portable local
volumes, distributed storage, NAT traversal, or control-plane high
availability. Those limits are part of the product, not hidden implementation
details.
