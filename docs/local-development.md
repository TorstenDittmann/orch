# Local development

`orch dev` runs an application on your own machine, from the same `orch.pkl`
you deploy to a cluster. It needs no cluster and no extra configuration: one
command starts a one-node cluster in a container and deploys the application
to it.

```console
orch dev
```

```text
Starting the dev node from ghcr.io/torstendittmann/orch-dev:0.2.0
Using local image profile:dev
Watching game generation 1 (rev_...)
HEALTHY  observed=1 active=1  all_health_gates_ready
Ready: game generation 1.

  http://website.game.localhost:8080
  http://profile.game.localhost:8080
  udp://127.0.0.1:27015 (server)

Inspect it with orch tui --target dev, or orch logs game SERVICE --target dev.
Stop it with orch dev down.
```

## Requirements

The `orch` CLI for your machine, from the
[latest release](https://github.com/TorstenDittmann/orch/releases/latest)
(`orch-darwin-arm64` on Apple silicon, `orch-darwin-amd64`, `orch-linux-amd64`
or `orch-linux-arm64`), and the [Pkl](https://pkl-lang.org) CLI that every
manifest command uses.

A running Docker-compatible engine: Docker Desktop, OrbStack or Colima on macOS,
or Docker Engine on Linux. `orch dev` uses `docker` when it is installed and
`podman` otherwise; set `ORCH_DEV_ENGINE` to choose. The engine runs a single
privileged container named `orch-dev`; nothing is installed on your machine.

## How it works

The `orch-dev` container runs the same controller, agent, containerd and Caddy
as a cluster node, so services behave as they do in production: service DNS,
health checks, rollouts, cron jobs, pre-deploy hooks, volumes and secrets all
work the same way. `orch dev` adds a target named `dev` for it, so every other
command works against it with `--target dev`:

```console
orch app status game --target dev
orch logs game website --target dev
orch tui --target dev
```

Run `orch dev` again after changing the manifest or rebuilding an image. Only
what changed is redeployed.

## What changes locally

`orch dev` deploys your manifest with these adjustments, and leaves everything
else as written:

| Manifest | Locally |
| --- | --- |
| `http.domains` | Replaced by `http://SERVICE.APP.localhost:8080` over plain HTTP. Browsers and `curl` resolve `*.localhost` to your machine without any `/etc/hosts` entries. |
| `http.public` | Also served at `http://localhost:8080` |
| `tcp.publicPort`, `tcp.nodePort`, `udp.nodePort` | Published on `127.0.0.1` with the same port number |
| `constraints` | Ignored, since there is only one node |
| Images that exist in your local engine | Copied into the dev node and pinned by digest instead of being pulled |

Use `--http-port` to serve HTTP on another port. Published ports are only
reachable from your machine.

## Images

Build an image with your usual tool and reference that tag in `orch.pkl`:

```console
docker build -t profile:dev ./profile
orch dev
```

When the local engine has an image with the reference the manifest uses,
`orch dev` uses that copy, the same way `docker compose up` does. Other images
are pulled from their registry as on a cluster; `orch registry login HOST
--target dev` stores credentials for private ones.

## Secrets

Put secret values in `orch.dev.env` next to the manifest, one `NAME=VALUE` per
line, and keep the file out of version control:

```text
# orch.dev.env
database-url=libsql://database.game.svc.internal:8080
session-key="a value with spaces"
```

`orch dev` sets new and changed secrets on every run; services that use a
changed secret restart with the new value. When the manifest references a
secret that is neither in the file nor already set, `orch dev` lists the
missing names and stops before deploying. Use `--env-file` to read another
file.

## Stopping and resetting

```console
orch dev down             # stop the dev node and keep its data
orch dev down --volumes   # also delete volumes, images and secrets
```

The dev node restarts with the engine, so the application comes back after a
reboot. Data in volumes is kept until `orch dev down --volumes`.

## Limits

- The dev node is one node: services with several replicas run them all on it,
  and node labels, draining and multi-node networking cannot be tried locally.
- CPU and memory requests are enforced. If the application requests more than
  the engine offers, raise the engine's resources (in Docker Desktop: Settings,
  Resources).
- HTTPS and certificates are not used locally.
