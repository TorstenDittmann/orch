# ADR 0001: expose an application platform, not a generic scheduler

Status: Accepted by Plan #1.

orch exposes applications, services, revisions, routes, secrets, and local
volumes. It does not expose pods, scheduler allocations, controllers, or
plugins as authoring primitives.

The narrow model keeps routine deployments understandable and gives the system
permission to provide strong defaults. Workloads needing arbitrary orchestration
primitives should use a broader platform.
