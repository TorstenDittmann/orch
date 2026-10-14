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

`orch status` also shows when the administrator and controller certificates
expire. Renew them before then; see
[certificate renewal](security.md#certificate-renewal).

`orch web` serves the same view as a page on a loopback address and opens it in
your browser. It prints a one-time URL; the page only works on this machine and
the administrator credentials stay in the `orch` process. Pass `--no-open` to
only print the URL, and `--port N` to choose the port.

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
