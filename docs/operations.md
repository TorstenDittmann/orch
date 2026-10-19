# Cluster operations

Use the CLI or TUI to inspect applications, manage nodes, run jobs, and maintain
the cluster.

Select a target before running commands:

```console
orch target list
orch target use production
orch target test
```

Pass `--target production` explicitly in automation.

## Cluster status

```console
orch status
orch nodes
orch tui
orch web
```

The TUI provides a keyboard-driven view of applications, instances, workers,
deployments, events, and live logs.

`orch web` opens a browser dashboard for the selected target: cluster health,
applications with replicas, routes, cron runs, deployment history, diffs and
rollback, runtimes, nodes with cordon, drain and ingress controls, one-off jobs,
events, and live logs. Press `⌘K` or `/` to jump anywhere.

The dashboard is served from the `orch` binary on a loopback address. It prints
a one-time URL; the page only works on this machine and the administrator
credentials stay in the `orch` process. Pass `--no-open` to only print the URL,
and `--port N` to choose the port.

`orch status` also shows when the administrator and controller certificates
expire. Renew them before then; see
[certificate renewal](security.md#certificate-renewal).

## Applications

```console
orch app status hello
orch app history hello
orch events --limit 50
orch logs hello web --replica 1 --tail 100
```

When a service is blocked, its status includes a reason such as missing
capacity, a label mismatch, a missing secret, or a lost local-volume worker.
Resolve that condition and orch will reconcile the desired state.

## Node labels

Labels let applications request specific workers:

```console
orch node label worker-1 region=eu-west
orch node label worker-1 tier=compute
orch nodes
```

Use the same keys in a service's `constraints`. The `orch.*` label prefix is
reserved.

## Node maintenance

Cordon a worker to stop new placement:

```console
orch node cordon worker-1
```

Drain movable workloads before maintenance:

```console
orch node drain worker-1
```

Services with local volumes remain pinned and are reported instead of being
moved. After maintenance:

```console
orch node uncordon worker-1
```

## One-off jobs

Run a command from a service definition:

```console
orch job run hello web -- ./bin/migrate up
orch job show JOB_ID
orch job logs JOB_ID
```

Use a pre-deploy hook in `orch.pkl` when a migration must finish before a new
revision rolls out:

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

Migration commands should be safe to retry.

## Scheduled jobs

```console
orch cron runs hello cleanup
orch cron logs hello cleanup RUN_ID
```

Overlapping occurrences are skipped. Failed and missed occurrences are not
automatically replayed.

## Removing applications

Inspect the application and its local data before removal:

```console
orch app status hello
orch remove hello
```

Removing desired state stops its workloads. Treat local-volume data separately
according to your retention policy.

## Monitoring

Every agent reports its node's CPU, memory, and disk usage with each
heartbeat, and the controller keeps the last 24 hours of it. `orch nodes`
prints the current usage, and the TUI and `orch web` show it with a 24-hour
chart per node. Nothing needs to be installed or enabled.

The controller serves Prometheus metrics at `/metrics`, computed from cluster
state on each scrape. There is nothing to enable. The endpoint takes the same
credentials as the administrator API: an administrator certificate on the
HTTPS port, or access to the local admin socket. Metrics name every node,
application and domain, so they are not public.

Scrape the HTTPS port with the `ca.pem`, `admin.pem` and `admin-key.pem` of an
administrator context (`orch init` keeps one in `/etc/orch/admin`). Protect
these files like any administrator credentials, and replace them when you
[renew the certificates](security.md#certificate-renewal):

```yaml
scrape_configs:
  - job_name: orch
    scheme: https
    tls_config:
      ca_file: /etc/prometheus/orch/ca.pem
      cert_file: /etc/prometheus/orch/admin.pem
      key_file: /etc/prometheus/orch/admin-key.pem
    static_configs:
      - targets: ["controller.example.com:9443"]
```

On the controller host, `curl --unix-socket /var/lib/orch-controller/admin.sock
http://orch/metrics` prints the same metrics as root.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `orch_build_info` | `version`, `build_id`, `go_version` | Always 1 |
| `orch_agent_sessions` | | Connected agents |
| `orch_nodes` | `lifecycle`, `connection` | Node count; `connection` is `connected`, `disconnected`, `unreachable` (no heartbeat for 45s) or `enrolled` |
| `orch_node_connected` | `node`, `lifecycle` | 1 when the node's agent is connected |
| `orch_node_certificate_expiry_seconds` | `node` | Until the worker certificate expires (renewed automatically 90 days ahead) |
| `orch_node_cpu_used_cores` | `node` | CPU cores in use, averaged over the last heartbeat interval |
| `orch_node_memory_used_bytes` | `node` | Memory in use, excluding reclaimable cache |
| `orch_node_disk_used_bytes`, `orch_node_disk_total_bytes` | `node` | Used and total bytes of the filesystem holding the agent's data |
| `orch_certificate_expiry_seconds` | `certificate` | Until the `ca`, `controller` or `admin` certificate expires; `admin` is the scraper's own certificate |
| `orch_application_status` | `application`, `status` | Always 1; `status` is e.g. `healthy`, `running`, `degraded`, `failed`, `blocked` |
| `orch_allocations` | `application`, `service`, `phase`, `health` | Instance count; `phase="backoff"` is crash-looping |
| `orch_allocation_restarts` | `application`, `service` | Restarts of the current instances |
| `orch_routes` | `application`, `kind`, `state` | HTTP and TCP routes per ingress node by `state` (`active`, `pending`, `failed`) |
| `orch_cron_job_last_run_failed` | `application`, `job` | 1 when the last run failed or timed out |
| `orch_controller_backup_timestamp_seconds` | | Unix time of the newest automatic controller backup |
| `orch_controller_backup_failed` | | 1 when the last automatic backup attempt failed |

Suggested alerts:

```yaml
groups:
  - name: orch
    rules:
      - alert: OrchNodeLost
        expr: orch_node_connected == 0
        for: 5m
      - alert: OrchCertificateExpiring
        expr: orch_certificate_expiry_seconds < 30 * 86400 or orch_node_certificate_expiry_seconds < 30 * 86400
      - alert: OrchApplicationUnhealthy
        expr: orch_application_status{status=~"degraded|failed|blocked"} == 1
        for: 10m
      - alert: OrchCrashLooping
        expr: orch_allocations{phase="backoff"} > 0
        for: 10m
      - alert: OrchRouteFailed
        expr: orch_routes{state="failed"} > 0
        for: 10m
      - alert: OrchControllerBackupStale
        expr: time() - orch_controller_backup_timestamp_seconds > 2 * 86400
```

## Backups

The controller backs itself up once a day, starting on its first start, and
keeps the newest 7 backups in `/var/lib/orch-controller/backups`. Each backup
is a directory named by its UTC time, such as `20261017T030000Z`, holding a
consistent copy of the database, the secret-encryption key, and the
controller identity. `orch status` shows the newest one and warns when it is
older than 2 days or the last attempt failed.

Those backups protect against a bad change or a damaged database, not against
losing the host. Copy them off the controller regularly, for example:

```console
sudo rsync -a /var/lib/orch-controller/backups/ backup-host:/srv/orch-backups/
```

Restore one into a new data directory, then start the controller from it as
described in [recovery](security.md#recovery):

```console
sudo orch cluster restore \
  --backup /var/lib/orch-controller/backups/20261017T030000Z \
  --data-dir /var/lib/orch-controller.restored
```

To keep a different number of daily backups, or none, pass
`--backups-keep N` to `orch controller serve` (`0` turns them off). For an
extra copy at any time:

```console
sudo orch cluster backup \
  --data-dir /var/lib/orch-controller \
  --output /secure/orch-backups/production.db \
  --secret-key-output /secure/orch-backups/production.key
```

Protect backups like administrator credentials: they contain controller state,
PKI, and the secret-encryption key. See [security and recovery](security.md)
before restoring a controller.

## Volume backups

Give a local volume a `backup` block and its worker archives it on that
schedule, keeping the newest `keep` backups:

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

The first backup is taken as soon as the worker gets the policy. A worker that
was down when a backup was due takes it when it comes back. `before` runs in
the service's container first, to flush or save state; if it fails, that
backup is skipped and retried an hour later. SQLite and libSQL databases in the
volume are copied with SQLite's online backup, so a running writer cannot leave
a torn copy. Other files are copied as they are, so prefer `before` to make an
application write a consistent snapshot.

Backups are kept on the worker in `/var/lib/orch-agent/volume-backups/APP/VOLUME`.

```console
orch volume list
orch volume backups game world
orch volume backup game world
orch volume restore game world 20261017T030000Z
orch volume download game world 20261017T030000Z
```

`orch volume list` shows `LAST BACKUP FAILED` with the reason when the last
scheduled backup failed. Restoring stops the service that mounts the volume,
swaps in the backup, and starts the service again; the previous contents are
kept as a `-before-restore` backup. Backups taken with `orch volume backup` or
before a restore are never pruned; delete them from the backup directory when
you no longer need them.

Like controller backups, these protect against mistakes and damaged data, not
against losing the worker. Copy them off the worker, for example with `rsync`,
or download them with `orch volume download`.

## Upgrades

Upgrade on the controller host only. Install the new orch there and run its
upgrade:

```console
curl -fsSL https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.sh | sh
sudo orch cluster upgrade --dry-run   # optional preview
sudo orch cluster upgrade
```

`orch cluster upgrade` upgrades the orch it runs from, unless `--binary FILE`
names another one. Once the controller is back, it upgrades the workers itself,
one node at a time. Each worker downloads the controller's binary over its
agent connection and runs `orch node upgrade` with it, so containers keep
running and a worker whose new agent does not come back rolls back on its own.
The next worker starts when the previous one reconnects on the new version.
Workers on another architecture get the matching release binary, which the
controller downloads from GitHub and checks against the release's
`SHA256SUMS`.

Follow the rollout with `orch nodes`. Each node shows its `agent=` version
and, while it upgrades or after it failed, `upgrade=`. A failed worker is
retried an hour later, or upgraded by hand with
`sudo orch node upgrade --binary ./new-orch` on that worker. Workers installed
by a version without controller-driven upgrades need that manual upgrade once.

Keep the recovery path reported by the controller upgrade until normal traffic
and a test deployment have succeeded.

Wait for every worker to report the new version before deploying a manifest
that uses a field the old version did not have, such as `stopGracePeriod` or a
volume `backup`. An older worker cannot accept such an assignment and keeps
running its previous one. `orch volume` and `orch restart` also need upgraded
workers.

## Controller outages

Workers continue running their last assigned workloads. New deployments,
scheduling, node changes, and secret changes wait. Restore the controller at
the same canonical HTTPS address so existing workers and operator contexts can
reconnect.
