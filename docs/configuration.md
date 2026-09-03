# v1alpha1 application configuration

Applications are typed Pkl modules. The default file is `orch.pkl`, and every
application amends the versioned schema embedded in the CLI:

~~~pkl
amends "orch:/v1alpha1/Application.pkl"

name = "hello"

services {
  ["web"] {
    image = "example/web@sha256:..."
  }
}
~~~

The schema fixes the resolved `schema` value to `v1alpha1`, supplies visible
defaults, closes every object against unknown properties, and enforces types
and constraints during evaluation. Run `orch schema` to print the
exact `Application.pkl` source shipped by the binary. Run `orch render` to
inspect the canonical JSON that will cross the control API.

Pkl evaluation happens only in operator commands. The controller and agents do
not invoke Pkl. They validate and reconcile the resolved canonical JSON, so Go
validation remains the network trust boundary even for clients that do not use
the official CLI.

## Top level

| Property | Default | Meaning |
| --- | --- | --- |
| `name` | required | Application DNS label, at most 63 characters. |
| `services` | required | Mapping of service name to service definition; at least one. |
| `cronJobs` | empty | Mapping of scheduled one-shot jobs. |
| `volumes` | empty | Named local volumes. |
| `deploy` | defaults below | Application rolling-update behavior. |

Do not set `schema` in an application module. It is fixed by the versioned base
module and remains present in rendered JSON.

## Service

| Property | Default | Meaning |
| --- | --- | --- |
| `image` | required | OCI image reference; whitespace is rejected. |
| `command` | image default | Replacement entry point as a Pkl listing. |
| `args` | image default | Command arguments as a Pkl listing. |
| `replicas` | `1` | Desired instances, from 1 through 100. |
| `restart` | `"always"` | `"always"`, `"on-failure"`, or `"never"`. |
| `resources.cpu` | `"100m"` | Whole CPUs or positive millicpu, such as `"500m"`. |
| `resources.memory` | `"128Mi"` | Positive `Ki`, `Mi`, `Gi`, or `Ti` value. |
| `http` | absent | Internal HTTP endpoint and optional public domains. |
| `tcp` | absent | Internal TCP endpoint and optional raw public listener. |
| `dependsOn` | empty | Services that must be available before this service starts. |
| `env` | empty | Plain environment variables. |
| `secretEnv` | empty | Environment variable to secret-name references. |
| `secretFiles` | empty | Secret-name to in-memory file projections. |
| `constraints` | empty | Required node label/value pairs. |
| `mounts` | empty | Named-volume mounts. |

An HTTP object requires `port`. Configuring any domains also requires
`healthPath`. `https` defaults to true. Wildcards are not supported by
v1alpha1.

~~~pkl
http = new Http {
  port = 8080
  domains {
    "www.example.com"
  }
  healthPath = "/healthz"
  https = true
}
~~~

`http` and `tcp` are mutually exclusive. A TCP endpoint always receives a TCP
readiness probe. `publicPort` additionally exposes a byte-transparent IPv4
listener on the node:

~~~pkl
tcp = new Tcp {
  port = 9418
  publicPort = 9418
}
~~~

Dependencies name services in the same application. They form a distinct,
acyclic graph. Startup waits for one RUNNING process-only replica or one READY
endpoint replica. Existing processes are not stopped when a dependency later
becomes unavailable.

## Scheduled jobs

Cron jobs use exactly five UTC fields and run one container per occurrence.
They do not have replicas, restart policies, endpoints, mounts, or secrets.

~~~pkl
cronJobs {
  ["backup-monitor"] {
    image = "registry.example/backup@sha256:..."
    schedule = "*/2 * * * *"
    command { "/bin/backup-monitor" }
    dependsOn { "database" }
    startDeadline = "30s"
    timeout = "10m"
  }
}
~~~

Overlapping occurrences are skipped, failed runs are not retried, and missed
occurrences are not replayed. `orch cron runs APP JOB` lists recent outcomes;
`orch cron logs APP JOB RUN_ID` reads retained runtime logs.

Plain and secret environment mappings may not define the same variable. Secret
values are set through a future controller API; application configuration
contains names only.

~~~pkl
secretEnv {
  ["DATABASE_URL"] = "database-url"
}

secretFiles {
  new SecretFile {
    source = "signing-key"
    target = "/run/secrets/signing-key"
  }
}
~~~

Secret file targets must be clean paths below `/run/secrets`.

## Local volumes

v1alpha1 supports only the local driver. A local volume may belong to one
service, and that service must have exactly one replica. Mount targets must be
absolute and may not replace protected paths such as `/proc`, `/sys`, `/dev`,
`/run/orch`, or the agent-generated `/etc/resolv.conf`.

~~~pkl
services {
  ["database"] {
    image = "postgres:17"
    mounts {
      new Mount {
        source = "database"
        target = "/var/lib/postgresql/data"
      }
    }
  }
}

volumes {
  ["database"] {}
}
~~~

These rules make the future scheduler's failure behavior explicit: the
allocation stays attached to the node holding the data.

## Deployment defaults

~~~pkl
deploy {
  maxSurge = 1
  maxUnavailable = 0
  healthDeadline = "2m"
  autoRollback = true
}
~~~

The availability values cannot both be zero. The health deadline must be from
10 seconds through one hour.

## Composition and sandbox

Pkl composition may amend or import local `.pkl` modules below its evaluation
root:

~~~pkl
// base.pkl contains a complete typed application.
amends "base.pkl"

services {
  ["web"] {
    replicas = 3
    env { ["APP_ENV"] = "production" }
  }
}
~~~

The default root is the real path of the entry file's directory. Pass
`--root DIR` to `deploy`, `validate`, `plan`, or `render` when shared modules
live in a parent directory. The entry and every file import must remain below
that root; symlink escapes fail closed.

orch permits Pkl standard-library modules, rooted local file modules, HTTPS
modules, and the embedded `orch:` schema. Plaintext HTTP, package,
project-package, module-path, REPL, and arbitrary external module readers are
unavailable. All resources remain denied, including environment variables,
external properties, files, HTTP(S), and packages. `PklProject` evaluator
settings and user/global evaluator settings are ignored, so configuration
cannot inspect the host or turn secret environment values into desired state.

HTTPS modules are fetched by the operator-side CLI and may use relative HTTPS
imports. Pin production URLs to an immutable commit or release instead of a
moving branch. The evaluation root does not restrict network destinations, and
a mutable remote module may resolve differently between validation, planning,
deployment, or retries.

~~~pkl
amends "https://example.com/orch/v1alpha1/Application.pkl"
~~~

Stock Pkl tools that support external readers can resolve the embedded schema
through the plumbing command `orch pkl reader`. The orch authoring commands
configure this reader automatically. After building `bin/orch`, the equivalent
stock-Pkl evaluation is:

~~~console
mise exec -- pkl eval \
  --external-module-reader "orch=bin/orch pkl reader" \
  --format json \
  examples/hello/orch.pkl
~~~

## Execution support in this slice

The schema accepts the complete `v1alpha1` contract so future intent remains
forward-compatible, but the runtime executes a deliberately smaller subset. An
executable service must use a public, digest-qualified OCI image and may use
command, args, plain environment variables, replicas, restart policy, CPU and
memory limits, declared local volumes, HTTP or TCP endpoints, dependency gates,
and cron jobs. HTTP endpoints
receive a private CNI address and a stable
`<service>.<application>.svc.internal` DNS name. Domains are published through
Caddy only after the readiness gate succeeds.

The controller stores valid but currently unsupported intent and reports the
application as `BLOCKED` with a stable reason:

| Intent or requirement | Reason |
| --- | --- |
| Image tag without a digest | `unsupported_image_tag` |
| Local/private registry requiring credentials | `private_registry_unsupported` |
| `secretEnv` or `secretFiles` | `secrets_unsupported` |
| Node constraints | `constraints_unsupported` |
| Non-Linux or non-amd64/arm64 node | `unsupported_platform` |
| Missing cgroups v2 | `cgroups_v2_required` |
| Missing compatible containerd | `containerd_unavailable` |
| Reported containerd version other than 2.3.4 | `unsupported_runtime_version` |
| Missing configured snapshotter | `snapshotter_unavailable` |
| Missing official CNI 1.1.0 support | `cni_unavailable` |
| Missing official CNI plugins 1.9.1 bundle | `cni_plugins_unavailable` |
| Missing nftables | `nftables_required` |
| Missing workload DNS service | `dns_unavailable` |
| Missing Caddy 2.11.3 ingress prerequisite | `caddy_unavailable` |
| Workload CIDR overlapping a host route | `network_cidr_conflict` |

An HTTP service without `healthPath` uses a TCP readiness probe. With a path,
the agent sends an HTTP GET directly to the workload address and port every two
seconds with a one-second timeout. Two consecutive successes make it `READY`;
three consecutive failures make it `UNHEALTHY`. Public routes include only
ready upstreams. `RUNNING` remains a process-only state for applications that
do not declare HTTP; networked HTTP applications become `HEALTHY` only when
their probes and requested routes are active.

## Canonical form, transport, and digests

Pkl supplies source-level defaults and types. orch then applies the existing Go
validation and normalization rules, normalizes whole CPU values to millicpu,
sorts domains and mount projections, and encodes deterministic JSON. The
displayed SHA-256 digest identifies this resolved contract, not Pkl source
layout, composition, import URLs, or remote source bytes. Equivalent resolved
modules therefore produce the same digest. Mutable HTTPS modules can produce a
different resolved contract, and therefore a different digest, on a later
evaluation.

`orch plan` compares a target module with an optional `--against` module and is
local/read-only. `orch deploy` sends the exact rendered canonical JSON to the
authenticated controller. The controller strictly decodes it, validates it,
and requires byte-for-byte canonical form before storing an immutable revision.
Pkl source and its import graph never cross the API.

## Migrating from TOML

This is a pre-1.0 hard cut: new CLIs and controllers do not accept TOML. Upgrade
the CLI and controller together, install the pinned Pkl tool with
`mise install`, convert source files, and compare `orch render` output and the
reported digest before deploying. Existing canonical desired state continues
to run and does not require a database migration.

| Former TOML field | Pkl property |
| --- | --- |
| `health_path` | `healthPath` |
| `secret_env` | `secretEnv` |
| `secret_files` | `secretFiles` |
| `read_only` | `readOnly` |
| `max_surge` | `maxSurge` |
| `max_unavailable` | `maxUnavailable` |
| `health_deadline` | `healthDeadline` |
| `auto_rollback` | `autoRollback` |

TOML tables become typed object amendments, maps become Pkl mappings with
`["key"] = value`, and arrays of objects become listings containing
`new ClassName { ... }`. No automated converter or dual-format window is
provided.
