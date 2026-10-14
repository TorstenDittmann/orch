# orch

orch is the working name for an opinionated application orchestrator for small
Linux clusters. It aims to provide Compose-like authoring with scheduling,
self-healing, encrypted networking, service discovery, and safe deployments,
without exposing the Kubernetes or Docker Swarm APIs.

> [!IMPORTANT]
> The repository contains the 1–20 node runtime plus remembered local targets
> and the built-in operator TUI. The first enrolled node is ingress-enabled for
> upgrade compatibility; additional nodes start compute-only until explicitly
> enabled.

Services and cron jobs accept image tags or digests. The controller resolves tags
once per deployment and records immutable digests; history shows the requested
reference, and retries and rollback reuse the recorded images. One-off jobs run
maintenance commands using service images and secrets; optional pre-deploy hooks
gate updates on successful migrations while current services keep serving.

## The contract

- One typed, versioned Pkl module describes an application and may compose
  rooted local modules and HTTPS modules.
- Unknown fields are errors rather than silently ignored configuration.
- Defaults are resolved before hashing or planning.
- Plans compare resolved application values rather than source layout.
- Secret values never belong in the manifest.
- Local volumes are explicit, may only be used by one-replica services, and
  remain pinned to the node that first hosts them.
- Node constraints are exact label matches. CPU and memory requests are hard
  placement reservations as well as runtime limits.

The runtime uses OCI images with containerd/runc and isolated network
namespaces. Each node owns an RFC1918 workload `/24`, nftables rules, and local
service DNS. A managed WireGuard full mesh connects those node subnets, while
ingress-enabled nodes proxy HTTP/HTTPS and raw TCP directly to ready local or
remote allocations. The controller is not in the application data path.

## Try the runtime

Install the pinned Go and Pkl toolchain and build the CLI:

~~~console
mise install
mise exec -- make build

bin/orch validate --root . --file examples/hello/orch.pkl
bin/orch plan --root . --file examples/hello/orch.pkl
bin/orch render --root . --file examples/hello/orch.pkl
bin/orch schema > /tmp/Application.pkl
~~~

For a persistent controller on a fresh **Ubuntu 24.04 LTS amd64** or **Fedora
Server 44 amd64** host, use the [guided controller installer](docs/installation.md#guided-controller-installation).
It installs a systemd service and produces portable administrator and recovery
directories. Copy both off the controller host before adding workers.
Managed controller and worker installations can later be upgraded from a
trusted local executable with `cluster upgrade` and `node upgrade`. See the
[controller-first procedure](docs/installation.md#upgrade-a-managed-installation).

For development, initialize controller state manually. The command installs
the administrator identity into a private local target named `lab` and selects it:

~~~console
bin/orch cluster init \
  --name lab \
  --server https://127.0.0.1:9443 \
  --data-dir "$PWD/.orch-controller"
~~~

Start the controller in one terminal:

~~~console
bin/orch controller serve \
  --data-dir "$PWD/.orch-controller" \
  --listen 127.0.0.1:9443
~~~

Create a short-lived join artifact with the selected target, transfer it
privately to the Linux execution host, and enroll the agent without copying
administrator credentials to that host:

~~~console
bin/orch node join create node-1 \
  --ttl 15m \
  --output "$PWD/node-1.join"

bin/orch agent init \
  --join-file "$PWD/node-1.join" \
  --data-dir /var/lib/orch-agent

# Before the first workload makes the network immutable, this optional command
# changes the default 10.88.0.0/16 workload CIDR.
bin/orch network configure \
  --cidr 10.96.0.0/16

sudo bin/orch agent serve \
  --data-dir /var/lib/orch-agent \
  --advertise-address 10.0.0.11 \
  --wireguard-port 51820 \
  --system-reserved-cpu 500m \
  --system-reserved-memory 512Mi \
  --containerd-socket /run/containerd/containerd.sock \
  --snapshotter overlayfs \
  --cni-bin-dir /opt/cni/bin \
  --caddy-binary /usr/bin/caddy \
  --ingress-user orch-ingress \
  --ingress-data-dir /var/lib/orch-ingress
~~~

For a fresh **Ubuntu 24.04 LTS amd64** or **Fedora Server 44 amd64** node,
[guided installation](docs/installation.md) supplies the pinned dependencies,
systemd services and enrollment with `orch node install`. Use `orch node doctor`
or `install --dry-run` first to inspect prerequisites and planned actions.

The agent host must be Linux `amd64` or `arm64` with cgroups v2, containerd
2.3.4, `containerd-shim-runc-v2`, `runc`, `net.ipv4.ip_forward=1`, nftables,
the official CNI plugins 1.9.1, and kernel WireGuard support. Ingress-enabled
nodes additionally run Caddy 2.11.3 as a dedicated non-root user; its
root-owned binary must carry `cap_net_bind_service=+ep`. The agent needs
root-equivalent permission for containerd, network namespaces, and its
uniquely named nftables table. See
[docs/networking.md](docs/networking.md) for network and ingress operations and
[docs/runtime.md](docs/runtime.md) for the exact support boundary.

For multiple nodes, create one join artifact per unique node name and run
`agent init` with a private data directory on each host. A join artifact is a
bearer credential: keep it mode `0600`, transfer it over a protected channel,
and revoke it immediately if it may have been disclosed. Their advertised
private IPv4 addresses must be mutually reachable, and host/provider firewalls
must permit the configured WireGuard UDP port (51820 by default). Manage
placement and ingress explicitly:

~~~console
bin/orch node label node-1 region=eu-west storage=fast
bin/orch node cordon node-1
bin/orch node drain node-1
bin/orch node ingress node-2 --enable
bin/orch node join list
bin/orch node join revoke JOIN_ID
~~~

See [docs/operations.md](docs/operations.md) before draining or removing a node;
local-volume and cron workloads deliberately have stricter handoff rules.

Use the selected target from another terminal. Start the keyboard-first
operator interface with `bin/orch tui`, or use the same connection through the
non-interactive commands:

~~~console
bin/orch tui

bin/orch status

bin/orch nodes

bin/orch deploy \
  --root . \
  --file examples/runtime/orch.pkl

printf '%s' "$APP_TOKEN" | bin/orch secret set runtime-demo token \
  --from-stdin

printf '%s' "$REGISTRY_TOKEN" | bin/orch registry login registry.example \
  --username robot --password-stdin

bin/orch app status runtime-demo
bin/orch app history runtime-demo
bin/orch app watch runtime-demo --timeout 10m

# Compare an older revision with current desired state, then restore it.
# Replace REVISION_ID with an ID from app history.
bin/orch app diff runtime-demo REVISION_ID
bin/orch rollback runtime-demo REVISION_ID --wait

bin/orch logs runtime-demo worker --replica 1 --tail 100

bin/orch exec runtime-demo worker --replica 1 -- /bin/echo ok

bin/orch remove runtime-demo

bin/orch events

bin/orch cluster backup \
  --data-dir "$PWD/.orch-controller" \
  --output "$PWD/backups/lab.db" \
  --secret-key-output "$PWD/offline-keys/lab-secret.key"
~~~

Deployment history retains image references, outcomes, and rollback sources.
In the TUI, press `d` from Applications to browse history, compare revisions,
watch replica progress, and confirm a rollback. `deploy --wait` waits for the
accepted revision; `app watch` resumes observation. Rollback creates a new
revision and never restores volume data or past secret values. See
[deployment operations](docs/operations.md#deployment-history-comparison-and-rollback).

The TUI includes a runtime workspace for every current service replica and
retained cron run. Browse it by application or node, inspect exact placement
and lifecycle state, and press Enter on a runtime to follow its bounded
stdout/stderr logs. Scrolling away from the bottom pauses visual following;
`G` returns to live output. The same workspace works over direct, WireGuard,
and SSH targets.

For an existing cluster, import its context once and stop passing the path on
every command:

~~~console
bin/orch target import production ./context.json --use
bin/orch target test production
~~~

Targets can use the controller directly, an address routed by an existing
WireGuard interface, or an existing OpenSSH host alias:

~~~console
bin/orch target transport production wireguard --address 10.20.0.10:9443
bin/orch target transport production ssh --host prod-controller
~~~

Use `--target staging` for a one-off override or `ORCH_TARGET=staging` in a
shell or script. `--context FILE` remains supported for compatibility.

Use `orch cluster backup inspect --backup FILE` to check a snapshot and
`orch cluster restore --backup FILE --pki-dir DIR --secret-key FILE --data-dir NEW_DIR`
to recover with separately saved controller identity and key material. Follow the
[controller recovery guide](docs/operations.md#inspect-and-restore-a-controller)
to preserve endpoint continuity and account for snapshot age.

Initialization is idempotent for identical inputs and never rotates existing
credentials. Backup publication refuses to overwrite a file. See
[docs/operations.md](docs/operations.md) and
[docs/security.md](docs/security.md) before using either command outside a
disposable environment.

`orch deploy` remains a durable acceptance operation. Its exact replayable
response is `PENDING`; use `orch app status` to observe asynchronous execution, or add `--wait` to
watch the accepted revision until ready.
Pass the reported idempotency key back with `--idempotency-key` when retrying
an uncertain request. Removal follows the same retry rule and preserves local
volume contents.

Compare two valid application files:

~~~console
bin/orch plan --against old.orch.pkl --file orch.pkl
~~~

Add `--json` to supported commands for stable machine-readable output. Exit
status 2 means invalid usage or configuration; status 1 is reserved for
operational failures.

## Executable example

~~~pkl
amends "orch:/v1alpha1/Application.pkl"

name = "runtime-demo"

services {
  ["worker"] {
    image = "docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"
    command { "/bin/sh" }
    args { "-c"; "echo $MESSAGE; echo diagnostic >&2; exec sleep 3600" }
    env { ["MESSAGE"] = "hello from orch" }
  }
}
~~~

The runtime examples demonstrate a preserved local volume and a readiness-gated
private HTTP service. See [examples/runtime/orch.pkl](examples/runtime/orch.pkl),
[examples/http/orch.pkl](examples/http/orch.pkl),
[examples/multinode/orch.pkl](examples/multinode/orch.pkl),
[examples/tcp-cron/orch.pkl](examples/tcp-cron/orch.pkl),
[examples/secrets/orch.pkl](examples/secrets/orch.pkl),
[docs/configuration.md](docs/configuration.md), and
[docs/product.md](docs/product.md).

## Development

~~~console
mise exec -- make generate
mise exec -- make check
~~~

The embedded Pkl schema and generated Protocol Buffer/Connect bindings are
verified with tools pinned in `mise.toml`; the same file pins Pebble for
isolated ACME verification. GitHub Actions CI rejects stale generated
output, Pkl or Go formatting differences, vet issues, race failures, and test
failures.
The real containerd/network/DNS/Caddy lifecycle is additionally available as
`make containerd-e2e` on a dedicated compatible Linux host. `make network-e2e`
checks the complete network slice and isolated-ACME prerequisites before
running it; see
[docs/runtime.md](docs/runtime.md#privileged-runtime-verification).

## Status and roadmap

Work is split into reviewable vertical slices:

1. Configuration and CLI contract.
2. Secure single-node controller/agent/runtime.
3. Single-node CNI networking, health gates, DNS, and HTTPS ingress.
4. Multi-node scheduling and WireGuard routing.
5. Secrets and operator workflows (implemented ahead of multi-node).
6. Packaging and release hardening.

The public name and open-source license are intentionally undecided until
before the first distributable release.
