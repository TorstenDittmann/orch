# Deploy your first HTTPS application

This guide starts with a managed controller and one worker, then deploys a
health-checked web service. Use fresh Ubuntu 24.04 LTS or Fedora Server 44 amd64
hosts with mutually reachable private addresses.

## 1. Install the controller

Copy a trusted Linux amd64 `orch` executable to the controller host. Choose the
HTTPS address that the worker and your workstation can reach:

```console
sudo ./orch cluster install \
  --name production \
  --server https://10.0.0.10:9443 \
  --listen 0.0.0.0:9443 \
  --admin-dir /root/orch-production-admin \
  --recovery-dir /root/orch-production-recovery
```

Allow TCP 9443 from trusted workers and operators. Copy both private output
directories off the host. Import the administrator context on your workstation:

```console
orch target import production ./orch-production-admin/context.json --use
orch target test
```

## 2. Join a worker

Create one expiring credential for the worker and transfer it with the trusted
executable:

```console
orch node join create worker-1 --ttl 1h --output worker-1.join
```

On the worker, use its mutually reachable private address:

```console
sudo ./orch node install \
  --advertise-address 10.0.0.11 \
  --join-file ./worker-1.join
```

Allow UDP 51820 between workers. For public ingress, route DNS to an
ingress-enabled node and allow TCP 80 and 443.

## 3. Describe the application

Create `orch.pkl`:

```pkl
amends "orch:/v1alpha1/Application.pkl"

name = "hello"

services {
  ["web"] {
    image = "ghcr.io/acme/hello:1.2.3"
    replicas = 1
    resources {
      cpu = "250m"
      memory = "128Mi"
    }
    http = new Http {
      port = 8080
      domains { "hello.example.com" }
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

The image must listen on port 8080 and return success from `/healthz`.

## 4. Validate, inspect, and deploy

```console
orch validate
orch plan
orch deploy --wait --target production
orch app status hello --target production
```

orch resolves image tags to immutable digests, waits for readiness, and records
the deployment result. A failed update automatically returns traffic to the
last healthy revision when `autoRollback` is enabled.

Continue with [application configuration](configuration.md), learn the
[runtime and rollout contract](runtime.md), or review
[deployment history and manual rollback](operations.md#deployment-history-comparison-and-rollback).
