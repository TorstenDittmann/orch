# Jobs and schedules

Not every workload runs forever. orch has three ways to run a process that
starts, does its work, and exits:

| Kind | Started by | Defined in | Typical use |
| --- | --- | --- | --- |
| One-off job | `orch job run` | The command line, using a service's settings | Data fixes, admin scripts, manual migrations |
| Pre-deploy hook | Every new revision | `deploy.preDeploy` in `orch.pkl` | Database migrations before a rollout |
| Cron job | A five-field UTC schedule | `cronJobs` in `orch.pkl` | Reports, cleanup, backups |

All three share one runtime path on the worker: an isolated container with its
own resources, a timeout, recorded state and exit code, and logs you can read
afterwards. None of them is retried automatically, because the first attempt
may already have changed external state. When a run fails, you decide whether
to run it again.

## Run states

Jobs and cron runs report the same states. The CLI prints them with a
`JOB_RUN_STATE_` prefix, for example `JOB_RUN_STATE_SUCCEEDED`.

| State | Meaning |
| --- | --- |
| `WAITING` | Accepted, waiting for its node, its dependencies, or its start |
| `RUNNING` | The process is running |
| `SUCCEEDED` | The process exited with code 0 |
| `FAILED` | Nonzero exit, cancellation, or an outcome that could not be confirmed |
| `TIMED_OUT` | The run exceeded its timeout, or a one-off job missed its start deadline |
| `SKIPPED` | A cron occurrence that did not start; see [overlap and missed runs](#overlap-and-missed-runs) |

Each run also carries a reason:

| Reason | Meaning |
| --- | --- |
| `assignment_pending` | The controller has placed the job and is waiting for the worker |
| `dependency_wait` | Waiting for the services in `dependsOn` to become available |
| `process_running` | Running |
| `exit_zero` | Exited successfully |
| `exit_nonzero` | Exited with a nonzero code; the exit code is recorded |
| `timeout_exceeded` | Stopped after its timeout |
| `start_deadline_exceeded` | Could not start before its start deadline |
| `overlap_forbidden` | Cron only: the previous occurrence was still running |
| `cancelled` | Cancelled with `orch job cancel` |
| `start_outcome_unknown` | The worker restarted while starting the process and could not confirm whether it ran |
| `runtime_object_missing` | The container disappeared before its result was recorded |

## One-off jobs

A one-off job runs a command with the settings of an existing service in the
current revision of an application: its image, plain and secret environment,
secret files, resources, placement constraints and dependencies. It does not
receive the service's ports or volume mounts.

### Run a job

Put the command after `--`:

```console
orch job run shop web --target production -- ./bin/rake orders:reindex
```

```text
Idempotency key: 6f1c0b9e5a7d4c2f9e8b1a3d5c7e9f01
job_3e8a...  shop/web  JOB_RUN_STATE_WAITING  assignment_pending
  revision=... image=ghcr.io/acme/shop@sha256:... node=... kind=manual
```

The first word after `--` replaces the image's entry point; the rest become
its arguments. Use a shell explicitly when you need shell features:

```console
orch job run shop web --target production -- /bin/sh -c 'bin/export > /tmp/out && wc -l /tmp/out'
```

Useful flags:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--wait` | off | Wait for the run to finish and exit non-zero if it did not succeed |
| `--wait-timeout` | `20m` | How long `--wait` waits |
| `--timeout` | `15m` | Process timeout, from 10s to 24h |
| `--revision` | current | Refuse to start unless this is still the application's current revision |
| `--idempotency-key` | generated | Make the request safe to retry |
| `--json` | off | Machine-readable output |

`--wait` is convenient in scripts and CI:

```console
orch job run shop web --wait --timeout 30m --wait-timeout 35m \
  --target production -- ./bin/rake orders:reindex
```

### Retrying the request safely

`orch job run` prints its idempotency key to stderr before it contacts the
controller. If the command is interrupted or the network fails, run it again
with the same key. The controller returns the existing run instead of starting
a second one:

```console
orch job run shop web --idempotency-key 6f1c0b9e5a7d4c2f9e8b1a3d5c7e9f01 \
  --target production -- ./bin/rake orders:reindex
```

### Inspect, wait, and read logs

```console
orch job list --target production
orch job list shop --limit 50 --target production
orch job show JOB_ID --target production
orch job wait JOB_ID --timeout 1h --target production
orch job logs JOB_ID --tail 500 --target production
```

- `orch job list` shows the newest jobs first, 20 by default and at most 100.
  When more exist, it prints `Next page: --before ID`.
- `orch job show` prints the state, reason, revision, image, node, kind
  (`manual` or `pre_deploy`) and, once finished, the exit code.
- `orch job wait` waits up to `--timeout` and exits non-zero unless the job
  succeeded.
- `orch job logs` prints the last `--tail` lines (default 100) of stdout to
  stdout and of stderr to stderr.

The TUI (press `o` on an application) and the `orch web` dashboard also list
one-off jobs.

### Cancel and retry

```console
orch job cancel JOB_ID --target production
orch job retry JOB_ID --target production
```

Cancelling stops the process and marks the run `FAILED` with reason
`cancelled`.

`orch job retry` creates a new run with a new ID that refers to the original.
It reuses the original image, command, arguments and secret versions, so a
retry repeats exactly what was attempted, even if the secrets were rotated
since. A retry is refused when:

- the original run succeeded (start a new run instead);
- the original run has not finished (cancel it and wait first);
- the application has a newer revision than the original run.

### Placement and limits

The controller places a job when you submit it, on the first connected, ready
worker that matches the service's `constraints` and has enough free CPU and
memory for the service's `resources`. If no worker qualifies, the request
fails instead of waiting. At most 32 one-off jobs can be active in the cluster
at once.

The job waits up to one minute for the services in `dependsOn` to become
available; after that it ends as `TIMED_OUT` with `start_deadline_exceeded`.

A job stays on its worker. If that worker disconnects, orch does not move the
job or start it elsewhere, because the process may already have run. When the
worker returns, it reports the real outcome. If it restarted while starting
the process and cannot tell whether it ran, the run fails with
`start_outcome_unknown` and you decide whether to retry.

Job records are kept on the controller. Logs and containers of finished jobs
stay on the worker, which keeps the newest 100 completed one-off runs.

### When to use orch exec instead

`orch exec` runs a command inside a running replica, which suits interactive
inspection:

```console
orch exec shop web --replica 1 --target production -- /bin/sh
```

Use `orch job run` when the work should run in its own container with its own
resources, be recorded, and keep its logs and exit code.

## Pre-deploy hooks

A pre-deploy hook is a one-off job that orch starts automatically for every
new revision of an application. The new revision rolls out only after the hook
exits with code 0. It is the standard place for database migrations.

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

| Field | Default | Rules |
| --- | --- | --- |
| `service` | required | A service in this manifest |
| `command` | required | 1 to 256 entries; replaces the image's entry point |
| `args` | empty | At most 256 entries |
| `timeout` | `"15m"` | 10s to 24h |

### What happens during a deployment

1. `orch deploy` stores the new revision.
2. The controller starts the hook with the named service's settings from the
   new revision, so it runs the new image with the new configuration.
3. While the hook runs, the application is `BLOCKED` with reason
   `pre_deploy_running`. All services and cron jobs of the previous revision
   keep running unchanged.
4. When the hook succeeds, the rollout proceeds as usual.

Because the old revision keeps serving while the hook runs, and keeps serving
if the rollout is rolled back, migrations must be compatible with the code
that is already running. Add columns and tables before code uses them, and
remove them only in a later release.

The hook runs once per revision, including revisions created by
`orch rollback` when the restored configuration has a hook. Rotating a secret
does not create a revision and does not run the hook.

If no worker has capacity for the hook, or a referenced secret is missing, the
application is `BLOCKED` with reason `pre_deploy_waiting` until the problem is
resolved.

### When the hook fails

If the hook exits non-zero, times out, or misses its start deadline, the
deployment is `FAILED` with reason `pre_deploy_failed`, and the previous
revision keeps running. `orch deploy --wait` reports the failure.

```console
orch app status shop --target production
orch job list shop --target production
orch job logs JOB_ID --target production
```

Fix the cause and then either:

- retry the same hook, which resumes the same deployment if it succeeds:

  ```console
  orch job retry JOB_ID --target production
  ```

- or deploy a corrected revision, which runs its own hook.

Deploying a newer revision while a hook runs requests cancellation of the old
hook. The new hook starts after the old one has stopped.

### Things to plan for

- The hook needs free CPU and memory for the service's `resources` in addition
  to the running replicas.
- The hook's `dependsOn` services must already be available within one minute,
  because the new revision's services do not start until the hook succeeds.
  If an application's database is one of its own services, deploy the
  database before you add a hook that depends on it.
- The hook does not receive the service's volume mounts.
- `orch deploy --wait` waits 10 minutes by default. For long migrations, raise
  it with `--timeout`.
- orch cannot guarantee that a migration ran exactly once. Use a migration tool
  that records what it applied and is safe to run again.

## Cron jobs

A cron job is a template for a scheduled one-shot run. It is defined under
`cronJobs` and deployed with the rest of the application.

```pkl
cronJobs {
  ["cleanup"] {
    image = "ghcr.io/acme/shop:2.3.0"
    schedule = "0 2 * * *"
    command { "./bin/rake" }
    args { "sessions:cleanup" }
    secretEnv {
      ["DATABASE_URL"] = "database-url"
    }
    dependsOn { "database" }
    timeout = "10m"
  }
}
```

Unlike a one-off job, a cron job has its own `image`, environment, secrets,
resources, constraints and dependencies. Every field is listed in the
[manifest reference](manifest.md#cronjob).

### Schedules

Schedules use exactly five fields, always in UTC:

```text
┌───────── minute (0-59)
│ ┌─────── hour (0-23)
│ │ ┌───── day of month (1-31)
│ │ │ ┌─── month (1-12)
│ │ │ │ ┌─ day of week (0-6, Sunday = 0)
│ │ │ │ │
0 2 * * *
```

| Schedule | Runs |
| --- | --- |
| `*/15 * * * *` | Every 15 minutes |
| `0 * * * *` | At the start of every hour |
| `30 2 * * *` | Every day at 02:30 UTC |
| `0 6 * * 1-5` | Weekdays at 06:00 UTC |
| `0 3 1 * *` | The first day of every month at 03:00 UTC |

Descriptors such as `@daily`, a sixth field for seconds, and `CRON_TZ=` or
`TZ=` prefixes are rejected by `orch validate`. Convert local times to UTC
yourself, and remember that daylight saving time does not move a UTC schedule.

### Where cron jobs run

Each cron job has one owner worker, chosen by the scheduler like a service
replica: it must match the job's `constraints` and have room for its
`resources`, which stay reserved on that worker. The owner keeps the schedule
and starts runs from its own copy of the assignment, so schedules continue
while the controller is unavailable.

Ownership is sticky. If the job must move, for example because its
constraints changed or the worker is drained, the old owner finishes any
active run and releases the schedule before the new owner takes over. A worker
that cannot be reached keeps the job until it is confirmed gone.

### Timeouts and deadlines

| Field | Default | Range | Meaning |
| --- | --- | --- | --- |
| `startDeadline` | `"1m"` | 1s to 1h | How long after the scheduled time a run may still start, for example while waiting for `dependsOn` |
| `timeout` | `"15m"` | 10s to 24h | How long a started run may take before it is stopped |

### Overlap and missed runs

- No overlap. If the previous run is still active when the next occurrence is
  due, the new occurrence is recorded as `SKIPPED` with `overlap_forbidden`.
- No catch-up. Occurrences missed while the owner was down or busy are not
  replayed. One that cannot start within `startDeadline` is `SKIPPED` with
  `start_deadline_exceeded`.
- No retry. A failed or timed-out run waits for the next occurrence.
- Exactly-once execution is not guaranteed. Make jobs safe to run twice, and
  safe to skip once.

If a job must run at least once a day, schedule it more often than that and
let it exit early when its work is already done.

### Inspect runs and logs

```console
orch cron runs shop cleanup --target production
```

```text
job_9c1f...  2026-10-14T02:00:00Z  JOB_RUN_STATE_SUCCEEDED  reason=exit_zero
job_47ab...  2026-10-13T02:00:00Z  JOB_RUN_STATE_FAILED  reason=exit_nonzero
```

Read the output of one run by its ID:

```console
orch cron logs shop cleanup RUN_ID --tail 500 --target production
```

`orch cron runs` shows 20 runs by default; `--limit` accepts 1 to 100. Each
worker keeps the latest 20 runs of each cron job, including their logs, and
removes older ones. Copy anything you need to keep longer into your own log
storage. The TUI and `orch web` also show recent cron runs.

There is no command to trigger a cron job outside its schedule. To run the
same work now, use `orch job run` with a service that has the same image, or
temporarily deploy a more frequent schedule.

## Reading local volumes from cron jobs

A cron job can mount a service's local volume read-only. This is the way to
back up data that lives on one worker, such as a database's data directory or
a SQLite file, next to the data itself.

```pkl
services {
  ["database"] {
    image = "postgres:17"
    secretEnv {
      ["POSTGRES_PASSWORD"] = "postgres-password"
    }
    tcp = new Tcp { port = 5432 }
    mounts {
      new Mount {
        source = "pgdata"
        target = "/var/lib/postgresql/data"
      }
    }
  }
}

volumes {
  ["pgdata"] {}
}

cronJobs {
  ["pgdata-archive"] {
    image = "ghcr.io/acme/backup:1.0"
    schedule = "0 3 * * *"
    mounts {
      new Mount {
        source = "pgdata"
        target = "/backup/source"
        readOnly = true
      }
    }
  }
}
```

The rules:

- Every mount must set `readOnly = true`. The worker also refuses a writable
  cron mount and binds the volume read-only.
- Each mounted volume must be declared under `volumes` and mounted by a
  service in the same application.
- All volumes mounted by one cron job must belong to the same service, so they
  are on the same worker.
- Each run is placed on the worker that holds the volume. The job's own
  `constraints` and `resources` still apply; if they exclude that worker, the
  job is blocked with `local_volume_pinned`.
- Until the owning service has been placed and its volume exists, the job is
  not scheduled and the application reports `cron_volume_pending`.

The service keeps writing while the job reads, so copying files can capture
them mid-write. Use a method that is consistent for your data, such as the
database's own backup tool or a SQLite online backup, rather than a plain file
copy of a busy database.

## Examples

### Database migrations

Run migrations from the application image before each rollout:

```pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "shop"

services {
  ["web"] {
    image = "ghcr.io/acme/shop:2.4.0"
    replicas = 2
    secretEnv {
      ["DATABASE_URL"] = "database-url"
    }
    http = new Http {
      port = 8080
      domains { "shop.example.com" }
      healthPath = "/healthz"
    }
  }
}

deploy {
  preDeploy = new PreDeployHook {
    service = "web"
    command { "./bin/migrate" }
    args { "up" }
    timeout = "10m"
  }
}
```

```console
orch plan
orch deploy --wait --timeout 20m --target production
```

To run a one-time data migration outside a release, use a one-off job and
wait for it:

```console
orch job run shop web --wait --timeout 2h --wait-timeout 2h \
  --target production -- ./bin/migrate data 2026_10_backfill_totals
```

### Nightly logical backup

When the database is reachable over the network, a cron job can stream a dump
to off-site storage. Here `pg_dump` connects to the `database` service through
its cluster DNS name:

```pkl
cronJobs {
  ["nightly-backup"] {
    image = "ghcr.io/acme/pg-backup:17"
    schedule = "15 1 * * *"
    command { "/bin/sh" }
    args {
      "-c"
      "pg_dump --format=custom \"$DATABASE_URL\" | aws s3 cp - \"s3://acme-backups/shop/$(date -u +%F).dump\""
    }
    env {
      ["AWS_REGION"] = "eu-west-1"
    }
    secretEnv {
      ["DATABASE_URL"] = "backup-database-url"
      ["AWS_ACCESS_KEY_ID"] = "backup-aws-key-id"
      ["AWS_SECRET_ACCESS_KEY"] = "backup-aws-secret"
    }
    dependsOn { "database" }
    resources {
      cpu = "500m"
      memory = "512Mi"
    }
    timeout = "2h"
  }
}
```

Store the secrets first:

```console
printf '%s' 'postgres://backup:...@database.shop.svc.internal:5432/shop' |
  orch secret set shop backup-database-url --from-stdin --target production
printf '%s' "$AWS_ACCESS_KEY_ID" |
  orch secret set shop backup-aws-key-id --from-stdin --target production
printf '%s' "$AWS_SECRET_ACCESS_KEY" |
  orch secret set shop backup-aws-secret --from-stdin --target production
```

Run it once right away to check it, then watch the scheduled runs:

```console
orch cron run shop nightly-backup --wait --target production
orch cron runs shop nightly-backup --target production
orch cron logs shop nightly-backup RUN_ID --target production
```

A backup is only useful if it can be restored. Test restores regularly, and
alert when a run fails, for example by having the job report success to an
external monitor as its last step.

### Archiving a SQLite file

For data that only exists in a local volume, mount the volume read-only and
use the database's own safe copy method:

```pkl
cronJobs {
  ["sqlite-backup"] {
    image = "ghcr.io/acme/sqlite-backup:1.0"
    schedule = "0 */6 * * *"
    command { "/bin/sh" }
    args {
      "-c"
      "sqlite3 'file:/data/app.db?mode=ro' \".backup /tmp/app.db\" && upload /tmp/app.db"
    }
    mounts {
      new Mount {
        source = "app-data"
        target = "/data"
        readOnly = true
      }
    }
    timeout = "30m"
  }
}
```

Here `upload` stands for whatever tool your image uses to ship the copy
off the worker. The `app-data` volume must be mounted by a service in the same
application.

See [cluster operations](operations.md#one-off-jobs) for a short overview and
the [CLI reference](cli.md) for every flag.
