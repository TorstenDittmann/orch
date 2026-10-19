# Tutorial: deploy a web app with a database

This tutorial picks up where the [quickstart](quick-start.md) ends. You will
deploy a small but complete application: a web service with two replicas
behind HTTPS, a PostgreSQL database that keeps its data on one worker, secrets
for the database credentials, a nightly cron job, and a migration that runs
before each rollout. Then you will plan, deploy, inspect, update, and roll
back the application, the same way you would in production.

Each step shows the exact commands. It takes about 30 minutes, most of it
waiting for images and certificates.

## What you will build

The application is called `notes` and has these parts:

| Part | Kind | Details |
| --- | --- | --- |
| `web` | Service | Two replicas of your web image on port 8080, published at `https://notes.example.com` |
| `database` | Service | One PostgreSQL 17 replica with a local volume, pinned to a node labeled `role=database` |
| `database` | Volume | Local directory that holds the PostgreSQL data |
| `database-password`, `database-url` | Secrets | Stored encrypted on the controller and injected as environment variables |
| `cleanup` | Cron job | Runs `./bin/notes cleanup` every night at 02:00 UTC |
| Pre-deploy hook | Hook | Runs `./bin/notes migrate` before each new revision rolls out |

The web service reaches the database through cluster DNS at
`database.notes.svc.internal`. Only the web service is public.

## Before you begin

You need:

- A cluster with a controller and at least one worker, set up as in
  [installation](installation.md). Two workers make the tutorial more
  interesting: one runs public traffic and the other holds the database.
- A target for the cluster on your workstation. This tutorial assumes it is
  named `production`.
- Pkl 0.32.1 on your workstation's `PATH`. `orch validate`, `orch plan`, and
  `orch deploy` evaluate `orch.pkl` with it. `mise install` in the orch
  repository provides it.
- A domain you control. This tutorial uses `notes.example.com`.
- An OCI image for the web application. This tutorial uses
  `ghcr.io/acme/notes` with tags `1.0.0` and `1.1.0`; replace it with your own.
  The image must:
  - listen for HTTP on port 8080;
  - return a 2xx or 3xx status from `/healthz` without depending on the
    database being reachable;
  - read the database connection string from `DATABASE_URL`;
  - provide `./bin/notes migrate`, which applies schema migrations and is
    safe to run more than once, and `./bin/notes cleanup`, the nightly task.

If your image is in a private registry, store its credentials first:

```console
printf '%s' "$REGISTRY_TOKEN" | orch registry login ghcr.io \
  --username robot --password-stdin
```

Select the target so you do not have to repeat `--target production` on every
command. In scripts, pass `--target` explicitly instead.

```console
orch target use production
orch target test
orch nodes
```

`orch nodes` lists each worker with its state, capacity, labels, and whether
it accepts ingress (`ingress-role=true`).

## Step 1: Prepare the nodes

### Choose the database node

A local volume lives on one worker. Choose which one by labeling it, and
constrain the database service to that label. Here `worker-2` holds the
database:

```console
orch node label worker-2 role=database
orch nodes
```

The `labels=` field for `worker-2` now shows `role=database`. With a single
worker, label that worker instead.

Without a constraint, orch places the database wherever capacity allows the
first time and then keeps it there because of its volume. The label makes the
choice explicit, so you know which host to back up.

### Check ingress and DNS

The first worker that joined the cluster accepts public traffic
automatically. To use another one, enable ingress on it:

```console
orch node ingress worker-1 --enable
```

Create a DNS A record for `notes.example.com` that points at the address of
each ingress-enabled worker, and allow TCP 80 and 443 to those workers in your
host and provider firewalls. Caddy on the ingress node requests the
certificate automatically, and the certificate authority must be able to reach
the node on those ports. Check the record from your workstation:

```console
dig +short notes.example.com
```

See [HTTPS](networking.md#https) for details.

## Step 2: Store the secrets

The database needs a password, and the web service needs a connection string
that includes it. Store both as application secrets. Values go through stdin,
so they never appear in your shell history or the process list:

```console
DB_PASSWORD="$(openssl rand -hex 24)"

printf '%s' "$DB_PASSWORD" | orch secret set notes database-password --from-stdin

printf '%s' "postgres://notes:$DB_PASSWORD@database.notes.svc.internal:5432/notes" \
  | orch secret set notes database-url --from-stdin

orch secret list notes
```

Secrets belong to an application by name, so you can store them before the
application exists. The controller encrypts them and delivers them only to
the workers that run a workload referencing them. `orch.pkl` refers to them by
name and can stay in source control.

The host name `database.notes.svc.internal` follows the pattern
`SERVICE.APPLICATION.svc.internal`. Every node runs a DNS server that answers
these names with the addresses of ready replicas. See
[service discovery](networking.md#service-discovery).

## Step 3: Write the application

Create a directory for the application and an `orch.pkl` file in it. The
sections below build the file piece by piece; the complete file follows.

### The web service

```pkl
amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "notes"

services {
  ["web"] {
    image = "ghcr.io/acme/notes:1.0.0"
    replicas = 2
    resources {
      cpu = "250m"
      memory = "256Mi"
    }
    env {
      ["APP_ENV"] = "production"
    }
    secretEnv {
      ["DATABASE_URL"] = "database-url"
    }
    dependsOn { "database" }
    http = new Http {
      port = 8080
      domains { "notes.example.com" }
      healthPath = "/healthz"
    }
  }
}
```

- `replicas = 2` runs two instances. orch spreads them across eligible
  workers when capacity permits, and sends traffic only to ready instances.
- `resources` is what the scheduler reserves on a node. `memory` is also the
  container's memory limit.
- `secretEnv` maps the environment variable `DATABASE_URL` to the stored
  secret `database-url`.
- `dependsOn` makes the web replicas wait until the database is available
  before they start.
- `http` publishes the service. With `domains`, orch routes
  `notes.example.com` through the ingress nodes. `https` defaults to `true`,
  so the route is served over HTTPS with an automatic certificate.
  `healthPath` is required when a service has domains. orch sends an HTTP GET
  to it every two seconds; see [health checks](runtime.md#health-checks).

### The database service and its volume

Add the database to `services` and declare its volume at the top level:

```pkl
  ["database"] {
    image = "docker.io/library/postgres:17-alpine"
    resources {
      cpu = "500m"
      memory = "1Gi"
    }
    env {
      ["POSTGRES_DB"] = "notes"
      ["POSTGRES_USER"] = "notes"
    }
    secretEnv {
      ["POSTGRES_PASSWORD"] = "database-password"
    }
    constraints {
      ["role"] = "database"
    }
    mounts {
      new Mount {
        source = "database"
        target = "/var/lib/postgresql/data"
      }
    }
    tcp = new Tcp {
      port = 5432
    }
  }
```

```pkl
volumes {
  ["database"] {}
}
```

- `constraints` is an exact match on node labels. The database only runs on
  the node you labeled `role=database`. If no node matches, the service stays
  blocked instead of running somewhere else.
- `mounts` attaches the local volume `database` at PostgreSQL's data
  directory. A service with a local volume must have exactly one replica,
  which is the default.
- `tcp` declares the port other services connect to and gives the database a
  TCP readiness check, so the web service waits until PostgreSQL accepts
  connections. It has no `publicPort`, so the database is not reachable from
  outside the cluster.

If the worker that holds the volume is lost, orch reports `volume_node_lost`
and does not start an empty database elsewhere. Back up the data with the
database's own tools; see [local volumes](configuration.md#local-volumes).

### The nightly cron job

```pkl
cronJobs {
  ["cleanup"] {
    image = "ghcr.io/acme/notes:1.0.0"
    schedule = "0 2 * * *"
    command { "./bin/notes" }
    args { "cleanup" }
    secretEnv {
      ["DATABASE_URL"] = "database-url"
    }
    dependsOn { "database" }
    timeout = "10m"
  }
}
```

The schedule uses five cron fields in UTC, so this runs every day at 02:00
UTC. A run that is still going when the next one is due causes that next one
to be skipped, and runs missed while a node was down are not replayed.
`timeout` stops a run that takes longer than ten minutes.

### Rollout settings

```pkl
deploy {
  maxSurge = 1
  maxUnavailable = 0
  healthDeadline = "2m"
  autoRollback = true
}
```

During an update, orch may start one extra web replica (`maxSurge = 1`) and
never takes a ready one away before its replacement is ready
(`maxUnavailable = 0`). Each new replica has two minutes to become ready. If
the update fails, `autoRollback` returns to the last healthy revision.

The database restarts differently. A replica with a local volume is always
stopped before its replacement starts, so the same data directory is never
used by two processes. Expect a short database interruption whenever its
definition changes.

### The complete file

```pkl
amends "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/v1alpha1/Application.pkl"

name = "notes"

services {
  ["web"] {
    image = "ghcr.io/acme/notes:1.0.0"
    replicas = 2
    resources {
      cpu = "250m"
      memory = "256Mi"
    }
    env {
      ["APP_ENV"] = "production"
    }
    secretEnv {
      ["DATABASE_URL"] = "database-url"
    }
    dependsOn { "database" }
    http = new Http {
      port = 8080
      domains { "notes.example.com" }
      healthPath = "/healthz"
    }
  }
  ["database"] {
    image = "docker.io/library/postgres:17-alpine"
    resources {
      cpu = "500m"
      memory = "1Gi"
    }
    env {
      ["POSTGRES_DB"] = "notes"
      ["POSTGRES_USER"] = "notes"
    }
    secretEnv {
      ["POSTGRES_PASSWORD"] = "database-password"
    }
    constraints {
      ["role"] = "database"
    }
    mounts {
      new Mount {
        source = "database"
        target = "/var/lib/postgresql/data"
      }
    }
    tcp = new Tcp {
      port = 5432
    }
  }
}

volumes {
  ["database"] {}
}

cronJobs {
  ["cleanup"] {
    image = "ghcr.io/acme/notes:1.0.0"
    schedule = "0 2 * * *"
    command { "./bin/notes" }
    args { "cleanup" }
    secretEnv {
      ["DATABASE_URL"] = "database-url"
    }
    dependsOn { "database" }
    timeout = "10m"
  }
}

deploy {
  maxSurge = 1
  maxUnavailable = 0
  healthDeadline = "2m"
  autoRollback = true
}
```

## Step 4: Validate and plan

Check the file:

```console
orch validate
```

```text
valid notes (2 services, 1 cron jobs) sha256:...
```

`orch validate` evaluates the Pkl file and checks every value, such as names,
ports, durations, and references to volumes and services, without contacting
the cluster. It reports each problem with its path, for example
`services.database.replicas: must be 1 when the service uses local volumes`.

Then look at the plan:

```console
orch plan
```

```text
Plan for notes (sha256:...)
+ application: "notes"
+ deploy: {"max_surge":1,"max_unavailable":0,"health_deadline":"2m","auto_rollback":true}
+ services.database: {...}
+ services.web: {...}
+ cron_jobs.cleanup: {...}
+ volumes.database: {...}

Plan: 6 to add, 0 to change, 0 to remove.
```

`orch plan` works locally. Without `--against`, everything is new. You will
compare two versions of the file in step 8. Use `orch render` to see the full
resolved JSON that would be sent to the controller.

## Step 5: Deploy

```console
orch deploy --wait
```

The controller resolves `ghcr.io/acme/notes:1.0.0` and `postgres:17-alpine` to
immutable digests, stores the result as revision 1, and starts the rollout.
With `--wait`, the command prints progress until the revision is ready or the
wait times out (ten minutes by default; change it with `--timeout`). Each
progress line shows the application status, then one line per replica with
its node, phase, and health.

In order, you will see:

1. The database is placed on `worker-2`, its image is pulled, and PostgreSQL
   initializes the empty volume with the password from `database-password`.
2. Once port 5432 accepts connections, the database is ready, and the web
   replicas, which depend on it, start.
3. Each web replica becomes ready after two successful `/healthz` checks. The
   route for `notes.example.com` is published once the service is healthy.

The command ends with `Ready: notes generation 1.` Interrupting it does not
cancel the deployment; resume watching with `orch app watch notes`.

`POSTGRES_PASSWORD` only takes effect when PostgreSQL initializes an empty
data directory. Changing the secret later does not change the password of an
existing database.

## Step 6: Run the first migration

The web service is running but the database is empty. Run the migration once
as a one-off job, using the web service's image and environment:

```console
orch job run notes web --wait -- ./bin/notes migrate
```

orch prints the job ID, runs the command on a worker, and waits for it to
finish. Read its output and check the result:

```console
orch job logs JOB_ID
orch job show JOB_ID
```

`orch job list notes` lists recent jobs for the application. In step 8 you
will make this migration run automatically before every rollout.

## Step 7: Check status and logs

### Application status

```console
orch app status notes
```

The output starts with a summary:

- `Desired` is the newest generation and revision you submitted, and
  `Observed` is the generation the controller has acted on.
- `Status` is the overall state, such as `EXECUTION_STATUS_HEALTHY` or
  `EXECUTION_STATUS_BLOCKED`, followed by a `Reason` and `Message` when there
  is something to explain.
- `Active` is the generation that is serving traffic.

Then it lists:

- one line per replica (`web/1`, `web/2`, `database/1`) with its phase,
  `health=`, private address, restart count, and `reason=`;
- one line per route, such as `route https://notes.example.com`, with its
  state, `certificate=`, and number of ready upstreams;
- one line per cron job with `next=`, the time of the next run.

Open the site:

```console
curl -I https://notes.example.com
```

Caddy requests the certificate when the route is published, so the first
requests can fail with a TLS error for a short while. If it keeps failing, see
[troubleshooting](troubleshooting.md).

### Logs

Read the last lines of each replica's output:

```console
orch logs notes web --replica 1 --tail 100
orch logs notes web --replica 2 --tail 100
orch logs notes database --replica 1 --tail 50
```

Replicas are numbered from 1, as in `orch app status`. Standard error is
written to your terminal's standard error.

For a live view of applications, replicas, events, and streaming logs, run
`orch tui` or open the browser dashboard with `orch web`.

### Events

```console
orch events --limit 20
```

Events record changes made to the cluster: applied deployments and rollbacks,
secret changes, node enrollment and updates, join tokens, and one-off jobs.
Use them to see what changed, and when, before a problem started.

### Run a command in a replica

`orch exec` runs a non-interactive command inside a running replica. Check the
database from inside its container:

```console
orch exec notes database --replica 1 -- psql -U notes -d notes -c 'select 1'
```

### Try the cron job

The cleanup job will first run at 02:00 UTC. To try the same command now, run
it as a one-off job:

```console
orch job run notes web --wait -- ./bin/notes cleanup
```

After the schedule has fired, list its runs and read the logs of one:

```console
orch cron runs notes cleanup
orch cron logs notes cleanup RUN_ID
```

## Step 8: Ship an update

Version `1.1.0` of the image includes a schema change. This time, make the
migration part of the rollout so it cannot be forgotten.

Keep a copy of the deployed file so you can compare against it:

```console
cp orch.pkl orch-deployed.pkl
```

Edit `orch.pkl`:

1. Change the web image and the cron job image to `ghcr.io/acme/notes:1.1.0`.
2. Add a pre-deploy hook to the `deploy` block:

```pkl
deploy {
  preDeploy = new PreDeployHook {
    service = "web"
    command { "./bin/notes" }
    args { "migrate" }
    timeout = "5m"
  }
  maxSurge = 1
  maxUnavailable = 0
  healthDeadline = "2m"
  autoRollback = true
}
```

The hook runs `./bin/notes migrate` as a one-off job with the new revision of
the `web` service: its new image, environment, and secrets. Replicas of the
new revision are only rolled out after the hook succeeds. While it runs, the
current replicas, including the database, keep running.

Because the hook holds back the whole application, it would not work on the
very first deploy of an application whose database is part of the same file:
the migration would run before the database exists. That is why step 6 ran
the first migration by hand.

Validate and compare the new file with the deployed one:

```console
orch validate
orch plan --against orch-deployed.pkl
```

```text
Plan for notes (sha256:...)
~ cron_jobs.cleanup.image: "ghcr.io/acme/notes:1.0.0" -> "ghcr.io/acme/notes:1.1.0"
+ deploy.pre_deploy: {"args":["migrate"],"command":["./bin/notes"],"service":"web","timeout":"5m"}
~ services.web.image: "ghcr.io/acme/notes:1.0.0" -> "ghcr.io/acme/notes:1.1.0"

Plan: 1 to add, 2 to change, 0 to remove.
```

The database is not in the plan. Deploy:

```console
orch deploy --wait
```

While the hook runs, `orch app status notes` shows the reason
`pre_deploy_running` and the job ID in its message. Follow the migration's
output with:

```console
orch job list notes
orch job logs JOB_ID
```

When the hook succeeds, orch replaces the web replicas one at a time: it
starts a `1.1.0` replica, waits for it to become ready, then stops a `1.0.0`
replica. Traffic moves only to ready replicas, so the site stays up.

If a migration fails, the deployment stops with the reason
`pre_deploy_failed` and nothing from the new revision is rolled out. Read the
job's logs, fix the cause, then run the hook again with
`orch job retry JOB_ID`, or deploy a corrected file.

If the new replicas never become ready within `healthDeadline`, orch rolls
back automatically because `autoRollback = true`, and `orch app history`
records the outcome.

## Step 9: Roll back

Suppose `1.1.0` passes its health checks but has a bug that users notice. List
the application's deployments:

```console
orch app history notes
```

Each line shows a generation, its revision ID, when it was created, its
outcome, and markers for the `current` and `active` generations. The images
of each revision follow, with tags resolved to digests.

Compare the revision you want to go back to with what is deployed now, then
roll back to it:

```console
orch app diff notes REVISION
orch rollback notes REVISION --wait
```

A rollback creates a new generation from the stored revision and reuses its
resolved image digests, so you get exactly the images that ran before, even if
the tag has since moved. It goes through the same rolling update and health
checks as a deploy.

Two things a rollback does not do:

- It does not undo database migrations. Write migrations so that the
  previous version of the application still works after they run, for example
  by adding columns before code uses them and removing them a release later.
- It does not change `orch.pkl`. Your file still says `1.1.0`, and the next
  `orch deploy` would roll forward again. Revert the change in your file or
  in source control before you deploy next.

## Step 10: Back up the database

The database volume lives only on `worker-2`. Back it up with PostgreSQL's
own tools. For example, a cron job can mount the volume read-only and be
placed on the same node, or a cron job can run `pg_dump` against
`database.notes.svc.internal` and send the dump off the host. See
[reading a local volume](configuration.md#reading-a-local-volume) for the
read-only mount.

Also back up the controller, which holds the application's revisions and
secrets; see [backups](security.md#backups).

## Clean up

To remove the application and stop its workloads:

```console
orch app status notes
orch remove notes
```

Removing an application does not delete the volume's data. It stays on
`worker-2` in `/var/lib/orch-agent/volumes/notes/database`; keep or delete it
according to your retention policy. Remove the secrets and the node
label if you no longer need them:

```console
orch secret delete notes database-url
orch secret delete notes database-password
orch node unlabel worker-2 role
```

## Next steps

- Read the [application manifest](manifest.md) reference for every field.
- Learn more about [secrets](secrets.md), [jobs and schedules](jobs.md), and
  [deployments](runtime.md).
- Keep [troubleshooting](troubleshooting.md) at hand for when a rollout does
  not go as planned.
