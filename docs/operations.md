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
| `orch_certificate_expiry_seconds` | `certificate` | Until the `ca`, `controller` or `admin` certificate expires; `admin` is the scraper's own certificate |
| `orch_application_status` | `application`, `status` | Always 1; `status` is e.g. `healthy`, `running`, `degraded`, `failed`, `blocked` |
| `orch_allocations` | `application`, `service`, `phase`, `health` | Instance count; `phase="backoff"` is crash-looping |
| `orch_allocation_restarts` | `application`, `service` | Restarts of the current instances |
| `orch_routes` | `application`, `kind`, `state` | HTTP and TCP routes per ingress node by `state` (`active`, `pending`, `failed`) |
| `orch_cron_job_last_run_failed` | `application`, `job` | 1 when the last run failed or timed out |

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
```

## Backups

Create backups regularly and copy them off the controller:

```console
sudo /opt/orch-controller/bin/orch cluster backup \
  --data-dir /var/lib/orch-controller \
  --output /secure/orch-backups/production.db \
  --secret-key-output /secure/orch-backups/production.key
```

Protect backups like administrator credentials: they contain controller state,
PKI, and the secret-encryption key. See [security and recovery](security.md)
before restoring a controller.

## Upgrades

Upgrade the controller first, then workers:

```console
sudo ./new-orch cluster upgrade --binary ./new-orch --dry-run --json
sudo ./new-orch cluster upgrade --binary ./new-orch

sudo ./new-orch node upgrade --binary ./new-orch --dry-run --json
sudo ./new-orch node upgrade --binary ./new-orch
```

Keep the recovery path reported by each operation until normal traffic and a
test deployment have succeeded.

## Controller outages

Workers continue running their last assigned workloads. New deployments,
scheduling, node changes, and secret changes wait. Restore the controller at
the same canonical HTTPS address so existing workers and operator contexts can
reconnect.
