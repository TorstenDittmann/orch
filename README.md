# orch

orch is the working name for an opinionated application orchestrator for small
Linux clusters. It aims to provide Compose-like authoring with scheduling,
self-healing, encrypted networking, service discovery, and safe deployments,
without exposing the Kubernetes or Docker Swarm APIs.

> [!IMPORTANT]
> The repository currently contains the v0.1 configuration and durable
> single-controller bootstrap foundation. It does not run containers or expose
> a controller API yet.

## The contract

- One strict, versioned TOML file describes an application.
- Unknown fields are errors rather than silently ignored configuration.
- Defaults are resolved before hashing or planning.
- Plans are semantic and deterministic.
- Secret values never belong in the manifest.
- Local volumes are explicit and may only be used by one-replica services.

The distributed runtime will use OCI images with containerd/runc, direct
WireGuard routing, local service DNS, and Caddy ingress. The controller will not
be in the application data path.

## Try the foundation

Install the pinned Go toolchain and build the CLI:

~~~console
mise install
mise exec -- make build

bin/orch validate --file examples/hello/orch.toml
bin/orch plan --file examples/hello/orch.toml
bin/orch render --file examples/hello/orch.toml
bin/orch schema > /tmp/orch.schema.json
~~~

Initialize local state for a single controller and create an administrator
context:

~~~console
bin/orch cluster init \
  --name lab \
  --server https://controller.example.com:9443 \
  --data-dir "$PWD/.orch-controller" \
  --context "$PWD/.orch-client/context.json"

bin/orch cluster backup \
  --data-dir "$PWD/.orch-controller" \
  --output "$PWD/backups/lab.db"
~~~

Initialization is idempotent for identical inputs and never rotates existing
credentials. Backup publication refuses to overwrite a file. See
[docs/operations.md](docs/operations.md) and
[docs/security.md](docs/security.md) before using either command outside a
disposable environment.

Compare two valid application files:

~~~console
bin/orch plan --against old.orch.toml --file orch.toml
~~~

Add --json to validate or plan for stable machine-readable output. Exit status
2 means invalid usage or configuration; status 1 is reserved for operational
failures.

## Example

~~~toml
schema = "v1alpha1"
name = "hello"

[services.web]
image = "ghcr.io/acme/hello:1.2.3"
replicas = 2

[services.web.resources]
cpu = "500m"
memory = "256Mi"

[services.web.http]
port = 8080
domains = ["hello.example.com"]
health_path = "/healthz"

[services.web.secret_env]
DATABASE_URL = "database-url"
~~~

See [docs/configuration.md](docs/configuration.md) for the complete contract and
[docs/product.md](docs/product.md) for the deliberately narrow product scope.

## Development

~~~console
mise exec -- make generate
mise exec -- make check
~~~

The checked-in JSON Schema is generated from the schema embedded in the Go
package. Native change.codes CI rejects stale generated output, formatting
differences, vet issues, race failures, and test failures.

## Status and roadmap

The product roadmap is captured in change.codes Plan #1. The durable bootstrap
slice is change.codes Plan #2. Work is split into reviewable vertical slices:

1. Configuration and CLI contract.
2. Secure single-node controller/agent/runtime.
3. Multi-node scheduling and reconciliation.
4. WireGuard networking, DNS, and HTTPS ingress.
5. Secrets and operator workflows.
6. Packaging and release hardening.

The public name and open-source license are intentionally undecided until
before the first distributable release.
