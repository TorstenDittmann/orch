# Application manifest reference

An application is described by one Pkl module, usually named `orch.pkl`, that
amends the versioned `v1alpha1/Application.pkl` schema. The schema is
embedded in the `orch` binary, so `orch` never downloads it; editors fetch the
same file from GitHub for completion and type checks. This page lists every field it defines, with
its type, default, and the rules that `orch validate` enforces on top of the
Pkl types.

For a guided introduction, start with [applications](configuration.md). For
the commands that read this file, see the [CLI reference](cli.md).

```pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "shop"

services {
  ["web"] {
    image = "ghcr.io/acme/shop:2.3.0"
    replicas = 2
    http = new Http {
      port = 8080
      domains { "shop.example.com" }
      healthPath = "/healthz"
    }
  }
}
```

## How a manifest is evaluated

`orch validate`, `orch render`, `orch plan`, and `orch deploy` all follow the
same steps on your workstation:

1. Pkl evaluates the module and applies the schema's types and defaults.
2. orch converts the result to its resolved model and runs its own
   validation. Some rules, such as "a domain may only be routed by one
   service", cannot be expressed in Pkl types and are only checked here.
3. orch normalizes the result and produces canonical JSON and a
   `sha256:` digest.

Only the canonical JSON is sent to the controller. Controllers and workers
never run Pkl. The controller decodes the JSON again, rejects unknown fields
or JSON that is not in canonical form, and repeats the validation.

Evaluation needs the `pkl` executable on your `PATH`, at exactly the version
pinned by orch (currently 0.32.1). The repository installs it with
`mise install`. Evaluation is limited to 10 seconds, and both the source file
and the resolved JSON are limited to 1 MiB.

### Unknown fields

The schema is closed. Assigning a property that does not exist is an
evaluation error, and Pkl suggests the closest name:

```console
$ orch validate
evaluate Pkl manifest "orch.pkl": –– Pkl Error ––
Cannot find property `replica` in object of type `orch.Application#Service`.
...
Did you mean any of the following?
replicas
```

Authoring commands exit with status `2` for evaluation and validation errors.

### Validation errors

When the Pkl types pass but orch's own rules do not, every problem is listed
with its path in the resolved model. Paths use the snake_case names of the
canonical JSON, not the camelCase names you write in Pkl:

```console
$ orch validate
manifest is invalid
  cron_jobs.cleanup.schedule: cron schedule must use five UTC fields without timezone directives or descriptors
  deploy: max_surge and max_unavailable cannot both be zero
  services.web: http and tcp endpoints cannot both be configured
  services.web.http.health_path: is required when public HTTP ingress or domains are configured
```

Pass `--json` to `orch validate` for machine-readable output.

### Defaults and normalization

Every field with a default is filled in before validation, so the resolved
manifest is always complete. `orch render` prints it:

```console
$ orch render
{
  "schema": "v1alpha1",
  "name": "shop",
  "services": {
    "web": {
      "image": "ghcr.io/acme/shop:2.3.0",
      "replicas": 2,
      "restart": "always",
      "resources": {
        "cpu": "100m",
        "memory": "128Mi"
      },
      ...
```

orch then normalizes values that have more than one spelling, so equivalent
sources produce the same digest:

- A whole CPU count becomes millicores: `cpu = "2"` renders as `"2000m"`.
- Domains, `dependsOn`, secret files (by target) and mounts (by target) are
  sorted.
- Empty lists and maps are omitted from the canonical JSON.

Because the digest covers the resolved result, refactoring a manifest into
shared modules, or reordering entries, does not change it.

### Plan

`orch plan` compares two local manifests. Without `--against`, everything is
shown as an addition. With `--against`, it lists the resolved fields that
differ:

```console
$ orch plan -f orch.pkl --against orch.previous.pkl
Plan for shop (sha256:60eeea1b...)
~ services.web.replicas: 2 -> 3
~ services.web.resources.cpu: "100m" -> "2000m"

Plan: 0 to add, 2 to change, 0 to remove.
```

Both files must describe the same application `name`. Defaults that you did
not write still appear in the plan, because the comparison uses the resolved
model. `orch plan` does not contact the cluster. To compare stored revisions on
the cluster, use `orch app diff`; see [deployments](runtime.md#rollbacks).

## Top level

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `schema` | `"v1alpha1"` | `"v1alpha1"` | Fixed. You do not set it. |
| `name` | DNS label | required | Application name, unique in the cluster |
| `services` | `Mapping<DnsLabel, Service>` | empty | At most 100; an application needs a service or a database |
| `cronJobs` | `Mapping<DnsLabel, CronJob>` | empty | At most 100 |
| `volumes` | `Mapping<DnsLabel, Volume>` | empty | At most 100 |
| `notifications` | `Listing<Notification>` | empty | At most 20; see [Notification](#notification) |
| `databases` | `Mapping<DnsLabel, Database>` | empty | At most 20; see [Databases](databases.md) |
| `deploy` | `Deployment` | defaults below | Rollout behavior and pre-deploy hook |
| `allowPrivateNetwork` | `Boolean` | `false` | Let workloads reach hosts on private networks such as the node's VPC; administrator credentials only. See [what workloads can reach](networking.md#what-workloads-can-reach) |

Across all services an application may request at most 200 replicas in total
and route at most 1000 domains in total.

### Shared types

| Type | Accepted values |
| --- | --- |
| DNS label | 1 to 63 characters: lowercase letters, digits and `-`; starts with a letter and does not end with `-` |
| Environment name | Starts with a letter or `_`, then letters, digits or `_` |
| Manifest string | At most 4096 bytes and no NUL bytes |
| Port | Integer from 1 to 65535 |
| Duration | A Go duration string such as `"30s"`, `"5m"` or `"1h30m"`. Each duration field has its own range. |

Application, service, cron job, volume and secret names all use the DNS label
rule. A service named `api` in application `shop` is reachable at
`api.shop.svc.internal`; see [service discovery](networking.md#service-discovery).

## Service

A long-running workload. Each entry under `services` is a `Service`.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `image` | String | required | Valid OCI reference, at most 1024 bytes. Not with `static`. |
| `static` | `Static?` | `null` | Serve a folder instead of an image; see [static sites](#static-sites) |
| `command` | `Listing<String>` | image default | At most 256 entries, each nonblank |
| `args` | `Listing<String>` | image default | At most 256 entries, each nonblank |
| `replicas` | Int | `1` | 1 to 100; must be `1` when the service mounts a volume |
| `restart` | String | `"always"` | `"always"`, `"on-failure"` or `"never"` |
| `resources` | `Resources` | `100m`, `128Mi` | See [resources](#resources) |
| `http` | `Http \| Listing<Http>` | `null` | See [Http](#http). Not together with `tcp`. |
| `tcp` | `Tcp?` | `null` | See [Tcp](#tcp). Not together with `http`. |
| `udp` | `Udp?` | `null` | See [Udp](#udp). Requires `http` or `tcp`. |
| `dependsOn` | `Listing<DnsLabel>` | empty | At most 64 distinct services of this application |
| `env` | `Mapping<EnvironmentName, String>` | empty | At most 256 entries |
| `secretEnv` | `Mapping<EnvironmentName, DnsLabel>` | empty | At most 256 entries; secret names |
| `connect` | `Mapping<EnvironmentName, DatabaseName>` | empty | At most 20; see [Databases](databases.md) |
| `secretFiles` | `Listing<SecretFile>` | empty | At most 64 |
| `constraints` | `Mapping<String, String>` | empty | At most 64 required node labels |
| `mounts` | `Listing<Mount>` | empty | At most 64 |
| `stopGracePeriod` | `String?` | `null` (10s) | Whole seconds from `"1s"` to `"10m"` |
| `privileged` | Boolean | `false` | See [privileged services](#privileged-services) |
| `capabilities` | `Listing<String>` | empty | Up to 41 distinct canonical Linux `CAP_*` names; administrator-only; not with `privileged` |
| `autoscale` | `Autoscale?` | `null` | See [autoscaling](#autoscaling). Leave `replicas` unset. |

### Image

Tags are resolved to immutable digests by the controller when a deployment is
accepted, so every replica of a revision runs the same image. A rollback reuses
the recorded digests. Images from private registries need a registry
credential; see [secrets and registry credentials](secrets.md).

### Static sites

A service with `static` serves a folder of files, such as a site generator's
build output, without a Dockerfile or a container engine. `orch deploy` packs
the folder into an image with a web server, uploads it to the controller, and
deploys it like any other service, with its replicas, domains, HTTPS and
rollbacks.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `dir` | String | required | Folder to serve, relative to the manifest. It must be inside the evaluation root (the manifest's folder unless `--root` is given) |
| `spa` | Boolean | `false` | Answer unknown paths with `/index.html`, for client-side routing |

```pkl
services {
  ["web"] {
    static = new Static { dir = "dist" }
    http { domains { "example.com" } }
  }
}
```

A static service gets an `http` endpoint on port 8080 with a health check, so
`http { ... }` only adds domains or other settings to it. A different
`healthPath` must be a file in the folder, such as `/` for its `index.html`. It
can't also set `image`, `command` or `args`.

Build the site before deploying; `orch deploy` serves whatever is in the
folder. Files and folders whose names start with a dot, such as `.env` or
`.git`, are left out, except `.well-known`. Links to files inside the folder
are followed; links to folders, hidden files, or anything outside it stop the
deploy. Without `spa`, unknown paths get
the folder's `404.html` with status 404, if it has one. Responses are
compressed, and an unchanged folder is not uploaded again.

The web server is the Caddy release that ingress nodes run, without a shell
or anything else, running as an unprivileged user. orch downloads it once
into your user cache directory and checks its pinned checksum.

### Stopping

When a replica is replaced, removed, or drained, orch sends it `SIGTERM` and
waits `stopGracePeriod` before it sends `SIGKILL`. The default is 10 seconds.
Give servers that save state on shutdown, such as game servers, more time:

```pkl
services {
  ["game"] {
    image = "ghcr.io/acme/game:1.4"
    stopGracePeriod = "2m"
  }
}
```

Changing it replaces the replica, like any other change to the service.

### Privileged services

Replicas default to no Linux capabilities, no new privileges, the
default seccomp profile, and no access to the node's devices. A service that
must start containers of its own, such as a Docker daemon that runs build or
function sandboxes, can set `privileged = true` to lift all of that:

```pkl
services {
  ["docker"] {
    image = "docker.io/library/docker:29-dind"
    privileged = true
    mounts {
      new Mount { source = "docker"; target = "/var/lib/docker" }
    }
  }
}
```

A privileged replica can take over the node it runs on, so only administrator
credentials may deploy an application that has one. Deploys with a deployer
certificate, an access token, or from GitHub are refused. Give the service a
local volume for its working state: a Docker daemon cannot run its overlay
storage on the container's own root filesystem.

### Linux capabilities

When an image needs specific startup permissions, prefer an explicit
`capabilities` list to privileged mode. For example, an init process that fixes
volume ownership, switches UID/GID and later signals the child process can use:

```pkl
services {
  ["media"] {
    image = "ghcr.io/acme/media:1.4"
    capabilities {
      "CAP_CHOWN"
      "CAP_KILL"
      "CAP_SETGID"
      "CAP_SETUID"
    }
  }
}
```

Only the listed capabilities are placed in the OCI bounding, permitted and
effective sets. No ambient or inheritable capabilities are added. The default
seccomp profile, `no-new-privileges`, blocked host devices and other confinement
remain in effect. An image may drop these permissions itself after startup.

Use exact Linux names including `CAP_`; unknown names, duplicates and `ALL` are
rejected. The list is sorted in canonical JSON, omitted when empty, and changing
it replaces the replica. It cannot be combined with `privileged = true`.

Some capabilities, such as `CAP_SYS_ADMIN`, can substantially weaken isolation
despite the remaining confinement. Deploying or rolling back to an application
with any capability grants requires administrator credentials, including when
the application otherwise permits scoped deployer tokens or GitHub deployment.
Upgrade the CLI, controller and all workers to a release supporting capabilities
before deploying a manifest that uses them.

### Autoscaling

With `autoscale`, the controller adds and removes replicas to keep the
service's average CPU use near a share of its CPU request:

```pkl
services {
  ["web"] {
    image = "ghcr.io/acme/web:1.4"
    resources { cpu = "500m"; memory = "256Mi" }
    autoscale = new Autoscale {
      min = 2
      max = 10
    }
  }
}
```

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `min` | Int | `1` | 1 to 100; the replica count of a first deploy |
| `max` | Int | required | `min` to 100; counts toward the 200-replica limit |
| `cpuPercent` | Int | `70` | 10 to 100; target average CPU use as a percent of `resources.cpu` |

Every 30 seconds the controller averages the last minute of CPU use of the
service's ready replicas, as reported by their nodes. When it is more than
10% away from the target, the replica count changes in proportion, at most
doubling at once. It adds replicas right away and removes them one at a time,
only after every evaluation in the last five minutes asked for fewer. It
waits while a deployment of the application is rolling out.

Scaling is not a deployment: it keeps the current revision, adds nothing to
history, and `orch plan` shows no change. A new deployment keeps the current
count within the new `min` and `max`. Each change is a `service.scaled`
event that says why, and `orch app status` shows the count, CPU use and target.

When no node has room for another replica, the service stays at the count that
fits and `orch app status` says it is limited. Add a node or lower the request.

`autoscale` is not available for services that mount a volume or publish
node-local ports. Since CPU requests are also CPU limits, a replica can never
use more than 100% of its request, so leave headroom below 100.

### Command and args

`command` and `args` follow the usual container rules:

- `command` set: the process is `command` followed by `args`. The image's
  entry point and default arguments are ignored.
- Only `args` set: the image's entry point runs with `args` in place of the
  image's default arguments.
- Neither set: the image's configuration is used as is.

```pkl
services {
  ["worker"] {
    image = "ghcr.io/acme/shop:2.3.0"
    command { "/app/bin/worker" }
    args { "--queue" "default" }
  }
}
```

### Restart

| Value | Behavior |
| --- | --- |
| `"always"` | Restart the process whenever it exits |
| `"on-failure"` | Restart only after a nonzero exit |
| `"never"` | Do not restart |

### Dependencies

`dependsOn` names other services in the same application. A dependency counts
as available when at least one of its replicas is running, or ready if it has
an endpoint. The gate applies before starts, restarts, replacements and
rollbacks. A later outage of the dependency does not stop a dependent that is
already running.

orch rejects dependencies that are unknown, duplicated, on the service itself,
or that form a cycle.

### Environment and secrets

`env` holds plain values. `secretEnv` maps an environment variable to the name
of a stored secret. `connect` maps an environment variable to one of the
application's [databases](databases.md) and sets it to that database's
connection URL. A variable can appear in only one of the three.

```pkl
env {
  ["APP_ENV"] = "production"
  ["LOG_LEVEL"] = "info"
}
secretEnv {
  ["DATABASE_URL"] = "database-url"
}
secretFiles {
  new SecretFile {
    source = "tls-key"
    target = "/run/secrets/tls.key"
  }
}
```

`SecretFile` has two fields:

| Field | Type | Rules |
| --- | --- | --- |
| `source` | DNS label | Name of a stored secret |
| `target` | String | Clean absolute path below `/run/secrets/`, unique per workload |

How secrets are stored, delivered and rotated is described in
[secrets and registry credentials](secrets.md).

### Constraints

Each entry is an exact node-label match that every replica requires:

```pkl
constraints {
  ["region"] = "eu-west"
}
```

Keys and values must be nonblank; keys are at most 256 bytes. Besides the
labels you set with `orch node label`, every node also has `orch.node` (its
name), `orch.os` and `orch.arch`. If no node matches, the service stays
blocked. See [placement](configuration.md#placement).

### Mounts

A `Mount` attaches a declared volume:

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `source` | DNS label | required | Must name an entry in `volumes` |
| `target` | String | required | Clean absolute path, unique per workload |
| `readOnly` | Boolean | `false` | Must be `true` in cron jobs |

The target cannot be `/` or lie in `/proc`, `/sys`, `/dev`, `/run/orch`, or
be `/etc/resolv.conf`. A volume can be mounted by only one service, and that
service must have `replicas = 1`.

## Resources

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `cpu` | String | `"100m"` | A positive whole count such as `"2"`, or millicores such as `"250m"` |
| `memory` | String | `"128Mi"` | A positive integer with `Ki`, `Mi`, `Gi` or `Ti` |

The scheduler reserves these amounts on the node, and memory is enforced as a
hard limit without swap. Decimal values such as `"0.5"` or units such as
`"512M"` are rejected.

## Http

An HTTP endpoint. Ingress nodes route the listed domains to ready replicas.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `port` | Port | required | Port the container listens on |
| `public` | Boolean | `false` | Catch-all plaintext listener on ingress port 80 |
| `domains` | `Listing<String>` | empty | At most 100 distinct hostnames |
| `healthPath` | String | `""` | Required when `public` or `domains` is set |
| `https` | Boolean | `true` | Obtain certificates and redirect HTTP to HTTPS |
| `basicAuth` | `BasicAuth?` | `null` | Ask for a username and password first |
| `redirects` | `Mapping<String, String>` | empty | Domains that redirect to one of `domains` |

Rules for `domains`:

- Lowercase, fully qualified hostnames with at least one dot. No trailing
  dots, IP addresses, `localhost`, or names under `svc.internal`.
- A domain may start with `*.` to match any one label, such as
  `*.example.com` for `shop.example.com`. Exact domains win over wildcards.
  See [wildcard domains](networking.md#wildcard-domains).
- Internationalized names must be written in their ASCII (`xn--`) form.
- A domain may be routed by only one service. Across applications, the
  application created first keeps the domain; a later one is blocked with
  `domain_conflict`.

Rules for `healthPath`: it must start with `/`, be at most 2048 bytes, and
contain no newline or NUL bytes. Without it, readiness uses a TCP connection
check, which is only allowed for services with no domains and no public
listener.

With `https = true`, a domain receives a certificate automatically and plain
HTTP requests are redirected with `308`. With `https = false`, the domain is
served over plain HTTP on port 80. The `public` catch-all listener is always
plain HTTP, and only one service in the cluster can own it.

```pkl
http = new Http {
  port = 8080
  domains {
    "shop.example.com"
    "www.shop.example.com"
  }
  healthPath = "/healthz"
}
```

See [networking](networking.md#https) for DNS and firewall requirements.

### Basic auth

`basicAuth` puts a username and password in front of every domain of the
service. Ingress answers `401` until the browser sends the right account, so
the service never sees an unauthenticated request. It needs `domains` served
over HTTPS and no `public` listener, so the password is never sent in the
clear.

| Field | Type | Rules |
| --- | --- | --- |
| `user` | String | 1–64 letters, digits, `.`, `_` or `-` |
| `passwordSecret` | DnsLabel | Name of the application secret that holds the password |

```pkl
http = new Http {
  port = 8080
  domains { "admin.example.com" }
  healthPath = "/healthz"
  basicAuth = new BasicAuth { user = "admin"; passwordSecret = "admin-password" }
}
```

```sh
printf '%s' "$ADMIN_PASSWORD" | orch secret set APP admin-password --from-stdin
```

The controller hashes the password with argon2id and only the hash reaches the
nodes. The hash is kept cheap so that requests with random passwords cannot
wear down a node's ingress, so use a long random password, for example from
`openssl rand -base64 24`, rather than a word someone could guess. Setting a
new value for the secret rolls the service like any other secret change.
Health checks go straight to the replica, so they don't need the password. `orch dev` leaves basic auth off.

### Redirects

`redirects` sends every request for a domain to one of the service's
`domains` with a permanent `308`, keeping the path and query. A redirected
domain gets its own certificate when `https = true`, follows the same rules as
`domains`, and can't also be in `domains`.

```pkl
http = new Http {
  port = 8080
  domains { "example.com" }
  healthPath = "/healthz"
  redirects { ["www.example.com"] = "example.com" }
}
```

### Several ports

A container that serves HTTP on more than one port, such as an API and a web
console, takes a listing of `Http` entries, each with its own `port` and
`domains`:

```pkl
http = new Listing {
  new Http {
    port = 9000
    domains { "s3.example.com" }
    healthPath = "/health"
  }
  new Http {
    port = 9001
    domains { "console.example.com" }
  }
}
```

The entries are one endpoint of the service. Readiness checks the first
entry, so only it takes `healthPath` and `public`. Every entry needs its own
port and at least one domain, may have its own `redirects`, and uses the same
`https`, `nodeLocal` and `basicAuth` as the first. A single `Http`, as above,
is the same as a listing with one entry. Under `orch dev`, each later entry is
served on `SERVICE-PORT.APP.localhost`.

## Tcp

A raw TCP endpoint.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `port` | Port | required | Port the container listens on |
| `publicPort` | `Port?` | `null` | Opened on every ingress node and proxied to ready replicas |
| `nodePort` | `Port?` | `null` | Published on the node that runs each replica |

`publicPort` cannot be 22, 53, 80, 443 or 9443, and two services cannot use
the same one. `nodePort` cannot be 22, 53, 80, 443 or 9443, and cannot reuse a
`publicPort` of another service in the application.

```pkl
tcp = new Tcp {
  port = 5432
  publicPort = 15432
}
```

## Udp

A UDP endpoint published on the node that runs each replica.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `port` | Port | required | Port the container listens on |
| `nodePort` | Port | required | Cannot be 53 or 51820 |

UDP is not health checked, so a service with `udp` must also have `http` or
`tcp`; its replica is published once that endpoint is ready.

Any service that sets `tcp.nodePort` or `udp` makes the whole application use
`deploy.maxSurge = 0`. Read [node-local TCP and UDP](networking.md#node-local-tcp-and-udp)
before using them.

```pkl
services {
  ["game"] {
    image = "ghcr.io/example/game:1.4.2"
    tcp = new Tcp {
      port = 7777
      nodePort = 7777
    }
    udp = new Udp {
      port = 7777
      nodePort = 7777
    }
  }
}

deploy {
  maxSurge = 0
  maxUnavailable = 1
}
```

## Volume

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `driver` | `"local"` | `"local"` | Only `local` exists in `v1alpha1` |
| `backup` | `VolumeBackup?` | `null` | The volume must be mounted by a service |

A local volume lives on one worker. It is created there by the first replica
of the service that mounts it and stays bound to that worker. orch never
creates an empty replacement elsewhere; see
[local volumes](configuration.md#local-volumes).

```pkl
volumes {
  ["database"] {}
}
```

### VolumeBackup

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `schedule` | String | `"0 3 * * *"` | Five UTC cron fields |
| `keep` | Int | `7` | 1 to 365 scheduled backups kept on the worker |
| `before` | `Listing<String>` | empty | Command run in the owning service's container first |

```pkl
volumes {
  ["world"] {
    backup = new VolumeBackup {
      schedule = "0 */6 * * *"
      keep = 8
      before { "rcon-cli"; "save-all" }
    }
  }
}
```

Changing `backup` never restarts the service. See
[volume backups](operations.md#volume-backups).

## Database

`Postgres`, `MySQL`, `MariaDB`, `MongoDB`, `Redis`, `Valkey`, `Dragonfly`,
`Garnet`, `ClickHouse` and `LibSQL` each run that
engine as a service of the same name, with a `NAME-data` volume, generated
credentials and optional scheduled backups. Names are at most 54 characters.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `version` | String | per engine | Tag of the official image |
| `image` | String? | `null` | Full image reference instead |
| `resources` | `Resources` | per engine | |
| `constraints` | `Mapping<String, String>` | empty | Node labels |
| `backup` | `DatabaseBackup?` | `null` | `new DatabaseBackup {}` backs up at 03:00 UTC and keeps 7 |

```pkl
databases {
  ["main"] = new Postgres {
    version = "17"
    backup = new DatabaseBackup {}
  }
  ["cache"] = new Redis {}
}
```

See [Databases](databases.md) for credentials, backups and restore.

## CronJob

A scheduled one-shot workload. Each entry under `cronJobs` is a `CronJob`.
Cron jobs are described in detail in [jobs and schedules](jobs.md).

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `image` | String | required | Valid OCI reference, at most 1024 bytes |
| `schedule` | String | required | Exactly five cron fields, evaluated in UTC |
| `command` | `Listing<String>` | image default | At most 256 entries |
| `args` | `Listing<String>` | image default | At most 256 entries |
| `resources` | `Resources` | `100m`, `128Mi` | Same rules as services |
| `env` | `Mapping<EnvironmentName, String>` | empty | At most 256 entries |
| `secretEnv` | `Mapping<EnvironmentName, DnsLabel>` | empty | At most 256 entries |
| `connect` | `Mapping<EnvironmentName, DatabaseName>` | empty | At most 20; see [Databases](databases.md) |
| `secretFiles` | `Listing<SecretFile>` | empty | At most 64 |
| `dependsOn` | `Listing<DnsLabel>` | empty | Services of this application |
| `constraints` | `Mapping<String, String>` | empty | Required node labels |
| `startDeadline` | Duration | `"1m"` | 1s to 1h |
| `timeout` | Duration | `"15m"` | 10s to 24h |
| `mounts` | `Listing<Mount>` | empty | Every mount `readOnly = true`; see below |

Schedule rules:

- Exactly five fields: minute, hour, day of month, month, day of week.
- Always UTC. `CRON_TZ=` and `TZ=` prefixes are rejected.
- Descriptors such as `@daily` and six-field expressions with seconds are
  rejected.
- No leading or trailing spaces; at most 256 bytes.

Cron mount rules:

- Each mount must set `readOnly = true`. The Pkl type rejects it otherwise.
- Each source must be a declared volume that a service in this application
  mounts.
- All mounts of one job must belong to the same service.
- A mount target cannot repeat another mount or secret file target.

```pkl
cronJobs {
  ["nightly-report"] {
    image = "ghcr.io/acme/reports:1.0"
    schedule = "30 2 * * *"
    args { "--since" "24h" }
    secretEnv {
      ["DATABASE_URL"] = "database-url"
    }
    timeout = "30m"
  }
}
```

## Notification

`Slack`, `Discord` and `Webhook` send an application's events to a webhook.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `url` | DNS label | required | Name of the application secret holding the webhook URL |
| `on` | `Listing<String>` | empty, meaning the failures | `deploy.succeeded`, `deploy.failed`, `deploy.rolled_back`, `backup.failed`, `cron.failed`, `replica.unhealthy` |

```pkl
notifications {
  new Slack { url = "slack-webhook" }
}
```

See [Notifications](notifications.md).

## Deployment

The `deploy` block controls rolling updates.

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `maxSurge` | Int | `1` | 0 to 100 extra replicas during a rollout |
| `maxUnavailable` | Int | `0` | 0 to 100 replicas that may be unavailable |
| `healthDeadline` | Duration | `"2m"` | 10s to 1h for replacements to become ready |
| `autoRollback` | Boolean | `true` | Restore the last healthy revision on failure |
| `autoUpdate` | Boolean | `false` | Redeploy when an image tag points to a new digest; see [automatic image updates](runtime.md#automatic-image-updates) |
| `preDeploy` | `PreDeployHook?` | `null` | Job that must succeed before rollout |

`maxSurge` and `maxUnavailable` cannot both be `0`. An application that
publishes node-local ports must set `maxSurge = 0`. See
[rollouts](configuration.md#rollouts) and [deployments](runtime.md).

### PreDeployHook

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `service` | DNS label | required | A service in this manifest |
| `command` | `Listing<String>` | required | 1 to 256 entries |
| `args` | `Listing<String>` | empty | At most 256 entries |
| `timeout` | Duration | `"15m"` | 10s to 24h |

The hook runs as a one-off job with the image, environment, secrets,
resources, constraints and dependencies of the named service from the new
revision. The new revision rolls out only after it exits with `0`. Details and
failure handling are in [jobs and schedules](jobs.md).

```pkl
deploy {
  preDeploy = new PreDeployHook {
    service = "web"
    command { "./bin/migrate" }
    args { "up" }
    timeout = "5m"
  }
}
```

## Composing manifests

Pkl modules can import other modules, so shared defaults and conventions live
in one place instead of being copied between applications.

### Local modules

Imports of local files are allowed as long as they stay inside the evaluation
root. The root defaults to the directory that contains the manifest. Symlinks
are resolved before the check.

A shared module can export functions or templates:

```pkl
// lib/web.pkl
import "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

function web(imageRef: String, domain: String): Application.Service = new {
  image = imageRef
  replicas = 2
  resources {
    cpu = "250m"
    memory = "256Mi"
  }
  http = new Application.Http {
    port = 8080
    domains { domain }
    healthPath = "/healthz"
  }
}
```

```pkl
// orch.pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

import "lib/web.pkl"

name = "shop"

services {
  ["web"] = web.web("ghcr.io/acme/shop:2.3.0", "shop.example.com")
}
```

> Name function parameters differently from the properties they set. Inside
> `new { image = image }`, `image` refers to the property being defined, which
> makes Pkl recurse until it overflows the stack.

Within one file, a `local` value and Pkl's amend syntax remove repetition:

```pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "shop"

local base = new Service {
  image = "ghcr.io/acme/shop:2.3.0"
  env { ["APP_ENV"] = "production" }
  secretEnv { ["DATABASE_URL"] = "database-url" }
}

services {
  ["web"] = (base) {
    replicas = 2
    http = new Http {
      port = 8080
      domains { "shop.example.com" }
      healthPath = "/healthz"
    }
  }
  ["worker"] = (base) {
    command { "/app/bin/worker" }
  }
}
```

For per-environment files, one manifest can amend another. Values that are
not overridden come from the base:

```pkl
// production.pkl
amends "orch.pkl"

services {
  ["web"] {
    replicas = 4
  }
}
```

When a manifest imports modules from a parent directory, set a wider root:

```console
orch validate -f apps/shop/orch.pkl --root .
orch deploy -f apps/shop/orch.pkl --root . --wait
```

A module outside the root fails with
`Refusing to load module ... because it is not within the root directory`.

### HTTPS modules

Modules can also be imported from `https:` URLs, which lets several
repositories share one set of conventions:

```pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

import "https://modules.example.com/orch/v3/web.pkl"
```

Pin these URLs to an immutable release or commit. The digest covers the
resolved manifest, not the source or the URLs, so a module that changes
between `orch plan` and `orch deploy` changes what is deployed. Imported
modules are trusted code that runs on your workstation and can make requests
to any HTTPS service it can reach.

### What evaluation may not do

To keep manifests reproducible and keep workstation data out of desired state,
evaluation denies:

- all resource reads, including `read("env:...")`, `read("prop:...")` and
  `read("file:...")`;
- plain `http:` modules, `package:` and project packages, and module paths;
- local files outside the evaluation root.

Your environment variables, external properties, Pkl settings and package
caches are not passed to the evaluator. To vary a value per environment, keep
one manifest per environment and amend a shared base, as shown above. Keep
sensitive values out of manifests entirely and use
[secrets](secrets.md).

## Schema and editor support

`orch schema` prints the embedded schema, the authoritative source for this
page:

```console
orch schema > Application.pkl
```

Editors with the Pkl extension (VS Code, IntelliJ, Neovim and others using
`pkl-lsp`) resolve the `amends` URL directly, so completion, documentation and
type errors work without any setup. `orch` itself never fetches that URL: it
evaluates against the schema embedded in the binary, so manifests work offline
and always match the CLI version you run. Stock `pkl eval` also resolves the
URL.

Manifests written as `amends "orch:/v1alpha1/Application.pkl"` keep working
with `orch`, but editors cannot resolve that scheme. Replace it with the URL
above to get editor support. The earlier
`https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl`
URL also keeps working.

`pkl eval` shows the evaluated module, but only `orch validate` applies orch's
additional rules. Use `orch render` to see exactly what would be sent to the
controller.
