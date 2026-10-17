# Quickstart

Set up a cluster and deploy a web service. Complete the
[installation requirements](installation.md#requirements) before you begin.

## Controller

Install `orch` on the controller. The script downloads the binary for the
host from the [latest release](https://github.com/TorstenDittmann/orch/releases/latest),
verifies its checksum and puts it in `/usr/local/bin`:

```console
curl -fsSL https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.sh | sh
```

Then run, as root:

```console
sudo orch init
```

Allow TCP 9443 from trusted workers and operators. Install the CLI on your
workstation:

```console
# Linux and macOS
curl -fsSL https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.sh | sh

# Windows (PowerShell)
irm https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.ps1 | iex
```

Then add a target that reaches the controller over SSH as root:

```console
orch target add production ssh://root@10.0.0.10
```

## Worker

Create a join token on your workstation (or as root on the controller):

```console
orch join-token worker-1
```

Install `orch` on the worker the same way and run the printed command there:

```console
sudo orch join orch1.eyJlIjoiaHR0cHM6Ly8xMC4wLjAuMTA6OTQ0MyIs...
```

Allow UDP 51820 between workers. For public ingress, route DNS to an
ingress-enabled node and allow TCP 80 and 443.

## Application

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

## Deployment

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
[deployment history and manual rollback](runtime.md#rollbacks).
