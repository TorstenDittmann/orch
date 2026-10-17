# Run applications, without adopting a platform

orch deploys containerized applications to a small Linux cluster. Configure,
release, and operate them from one CLI.

> orch is in early development. Review the [current limits](product.md) before
> using it in production.

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

Setting up a cluster takes three commands, in the style of Docker Swarm:

```console
$ sudo orch init                          # on the controller
$ orch join-token worker-1                  # prints the join command
$ orch target add production ssh://root@controller.example.com
```

Your SSH key is the credential. There are no certificate bundles to copy
between machines.

## Start here

1. [Set up a controller and join workers](installation.md).
2. [Deploy your first HTTPS application](quick-start.md).
3. Learn how [rollouts, placement, and recovery](runtime.md) behave.

The controller stores desired state and computes placement. Agents reconcile
workloads through containerd and runc, connect node networks with WireGuard,
and route traffic directly through ingress nodes. Application traffic never
passes through the controller.

## Features

| Area | Product behavior |
| --- | --- |
| Deployments | Immutable plans, readiness gates, availability budgets, history, and rollback |
| Runtime | OCI services, workers, cron schedules, one-off jobs, hooks, and local volumes |
| Setup | `orch init` and `orch join` with one-time join tokens, SSH targets, and host checks that name each blocker and its fix |
| Network | Private per-node subnets, service DNS, a WireGuard mesh over private or public addresses, HTTPS and TCP ingress, node-local TCP and UDP ports |
| Operations | Remembered targets, live logs, events, runtime inventory, a keyboard-first TUI, and the `orch web` dashboard |
| Recovery | Durable agent assignments, controller backups, certificate renewal, and managed upgrades |

## Limits

orch targets small teams operating Linux hosts that can reach each other. It does not
provide Kubernetes APIs, multi-region control planes, distributed storage,
autoscaling, automatic fencing, or control-plane high availability. Those
limits keep routine deployment and recovery understandable.

[Read the product scope](product.md) or browse the
[source on GitHub](https://github.com/TorstenDittmann/orch).
