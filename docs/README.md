# Run applications, without adopting a platform

orch deploys ordinary OCI applications across **1–20 Linux servers**. You get
typed configuration, health-gated releases, automatic rollback, private
networking, service discovery, HTTPS ingress, jobs, secrets, and a focused
operator TUI in one executable.

> orch is in early development. It uses one durable controller. Agents keep
> their assigned workloads running during a controller outage, while new
> deployments and scheduling wait for the controller to return.

```pkl
amends "orch:/v1alpha1/Application.pkl"

name = "api"

services {
  ["web"] {
    image = "ghcr.io/acme/api:1.4"
    replicas = 2
    resources {
      cpu = "500m"
      memory = "256Mi"
    }
    http = new Http {
      port = 8080
      domains { "api.example.com" }
      healthPath = "/healthz"
    }
  }
}

deploy {
  maxSurge = 1
  maxUnavailable = 0
  autoRollback = true
}
```

```console
$ orch validate
$ orch plan
$ orch deploy --wait --target production
```

## Start here

1. [Install a controller and worker](installation.md) on Ubuntu 24.04 LTS or
   Fedora Server 44.
2. [Deploy your first HTTPS application](quick-start.md).
3. Learn how [rollouts, placement, and recovery](runtime.md) behave.

The controller stores desired state and computes placement. Agents reconcile
workloads through containerd and runc, connect node networks with WireGuard,
and route traffic directly through ingress nodes. Application traffic never
passes through the controller.

## What is included

| Area | Product behavior |
| --- | --- |
| Deployments | Immutable plans, readiness gates, availability budgets, history, and rollback |
| Runtime | OCI services, workers, cron schedules, one-off jobs, hooks, and local volumes |
| Network | Private per-node subnets, service DNS, WireGuard routing, HTTPS and raw TCP ingress |
| Operations | Remembered targets, live logs, events, runtime inventory, and a keyboard-first TUI |
| Recovery | Durable agent assignments, controller backups, guided host installation, and managed upgrades |

## Designed for a clear boundary

orch targets small teams operating mutually reachable Linux hosts. It does not
provide Kubernetes APIs, multi-region control planes, distributed storage,
autoscaling, automatic fencing, or control-plane high availability. Those
limits keep routine deployment and recovery understandable.

[Read the product scope](product.md) or browse the
[source on change.codes](https://change.codes/torsten/better-swarm).
