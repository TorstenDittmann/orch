# orch

orch is the working name for an opinionated application orchestrator for small
Linux clusters. It aims to provide Compose-like authoring with scheduling,
self-healing, encrypted networking, service discovery, and safe deployments,
without exposing the Kubernetes or Docker Swarm APIs.

> [!IMPORTANT]
> The repository currently contains a networked single-node execution slice.
> One administrator-enrolled Linux agent runs digest-pinned OCI
> workloads through containerd 2.3.4/runc, attaches isolated CNI networks,
> publishes `svc.internal` DNS, gates traffic on readiness, and manages Caddy
> ingress. Encrypted application secrets and exact-host private-registry
> credentials are supported. Multi-node scheduling remains unavailable.

## The contract

- One typed, versioned Pkl module describes an application and may compose
  rooted local modules and HTTPS modules.
- Unknown fields are errors rather than silently ignored configuration.
- Defaults are resolved before hashing or planning.
- Plans compare resolved application values rather than source layout.
- Secret values never belong in the manifest.
- Local volumes are explicit and may only be used by one-replica services.

The runtime uses OCI images with containerd/runc and isolated network
namespaces. The single node owns an RFC1918 workload subnet, nftables rules,
local service DNS, and a pinned external Caddy process. The controller is not
in the application data path. A later multi-node slice may connect node
subnets with WireGuard without changing the application contract.

## Try the single-node runtime

Install the pinned Go and Pkl toolchain and build the CLI:

~~~console
mise install
mise exec -- make build

bin/orch validate --root . --file examples/hello/orch.pkl
bin/orch plan --root . --file examples/hello/orch.pkl
bin/orch render --root . --file examples/hello/orch.pkl
bin/orch schema > /tmp/Application.pkl
~~~

Initialize local state for a single controller and create an administrator
context:

~~~console
bin/orch cluster init \
  --name lab \
  --server https://127.0.0.1:9443 \
  --data-dir "$PWD/.orch-controller" \
  --context "$PWD/.orch-client/context.json"
~~~

Start the controller in one terminal:

~~~console
bin/orch controller serve \
  --data-dir "$PWD/.orch-controller" \
  --listen 127.0.0.1:9443
~~~

On the Linux execution host, enroll the one supported agent while the
controller is running:

~~~console
bin/orch agent init \
  --name node-1 \
  --context "$PWD/.orch-client/context.json" \
  --data-dir /var/lib/orch-agent

# Before the first workload makes the network immutable, this optional command
# changes the default 10.88.0.0/16 workload CIDR.
bin/orch network configure \
  --cidr 10.96.0.0/16 \
  --context "$PWD/.orch-client/context.json"

sudo bin/orch agent serve \
  --data-dir /var/lib/orch-agent \
  --containerd-socket /run/containerd/containerd.sock \
  --snapshotter overlayfs \
  --cni-bin-dir /opt/cni/bin \
  --caddy-binary /usr/bin/caddy \
  --ingress-user orch-ingress \
  --ingress-data-dir /var/lib/orch-ingress
~~~

The agent host must be Linux `amd64` or `arm64` with cgroups v2, containerd
2.3.4, `containerd-shim-runc-v2`, `runc`, `net.ipv4.ip_forward=1`, nftables,
the official CNI plugins 1.9.1, and Caddy 2.11.3. Caddy runs as the dedicated
non-root user and its root-owned binary must carry
`cap_net_bind_service=+ep`. The agent needs root-equivalent permission for
containerd, network namespaces, and its uniquely named nftables table. See
[docs/networking.md](docs/networking.md) for network and ingress operations and
[docs/runtime.md](docs/runtime.md) for the exact support boundary.

Use the generated mTLS context from another terminal:

~~~console
bin/orch status --context "$PWD/.orch-client/context.json"

bin/orch nodes --context "$PWD/.orch-client/context.json"

bin/orch deploy \
  --root . \
  --file examples/runtime/orch.pkl \
  --context "$PWD/.orch-client/context.json"

printf '%s' "$APP_TOKEN" | bin/orch secret set runtime-demo token \
  --from-stdin --context "$PWD/.orch-client/context.json"

printf '%s' "$REGISTRY_TOKEN" | bin/orch registry login registry.example \
  --username robot --password-stdin \
  --context "$PWD/.orch-client/context.json"

bin/orch app status runtime-demo \
  --context "$PWD/.orch-client/context.json"

bin/orch logs runtime-demo worker --replica 1 --tail 100 \
  --context "$PWD/.orch-client/context.json"

bin/orch exec runtime-demo worker --replica 1 \
  --context "$PWD/.orch-client/context.json" -- /bin/echo ok

bin/orch remove runtime-demo \
  --context "$PWD/.orch-client/context.json"

bin/orch events \
  --context "$PWD/.orch-client/context.json"

bin/orch cluster backup \
  --data-dir "$PWD/.orch-controller" \
  --output "$PWD/backups/lab.db" \
  --secret-key-output "$PWD/offline-keys/lab-secret.key"
~~~

Initialization is idempotent for identical inputs and never rotates existing
credentials. Backup publication refuses to overwrite a file. See
[docs/operations.md](docs/operations.md) and
[docs/security.md](docs/security.md) before using either command outside a
disposable environment.

`orch deploy` remains a durable acceptance operation. Its exact replayable
response is `PENDING`; use `orch app status` to observe asynchronous execution.
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
isolated ACME verification. Native change.codes CI rejects stale generated
output, Pkl or Go formatting differences, vet issues, race failures, and test
failures.
The real containerd/network/DNS/Caddy lifecycle is additionally available as
`make containerd-e2e` on a dedicated compatible Linux host. `make network-e2e`
checks the complete network slice and isolated-ACME prerequisites before
running it; see
[docs/runtime.md](docs/runtime.md#privileged-runtime-verification).

## Status and roadmap

The product roadmap is captured in change.codes Plan #1. Durable bootstrap is
Plan #2, the authenticated control API is Plan #3, and encrypted secrets and
registry authentication are Plan #8. Work is split into reviewable vertical
slices:

1. Configuration and CLI contract.
2. Secure single-node controller/agent/runtime.
3. Single-node CNI networking, health gates, DNS, and HTTPS ingress.
4. Multi-node scheduling and WireGuard routing (next).
5. Secrets and operator workflows (implemented ahead of multi-node).
6. Packaging and release hardening.

The public name and open-source license are intentionally undecided until
before the first distributable release.
