# CLI reference

Everything in orch runs through one executable, `orch`. The same binary sets
up controllers and workers, deploys applications, and opens the TUI and the web
dashboard. This page lists every command the CLI accepts, grouped by task, with
its flags and their defaults.

Run any command with `--help` to print its flags, for example
`orch deploy --help`. `orch help` prints the full usage summary.

## Conventions

### Selecting a cluster

Commands that talk to a controller (remote commands) need to know which
cluster to use. orch picks one in this order:

1. `--target NAME` on the command line (also written `--target=NAME`).
2. The `ORCH_TARGET` environment variable.
3. The selected target, set with `orch target use` or `orch target add`.
4. On the controller host, when no target is selected, the local administrator
   socket at `/var/lib/orch-controller/admin.sock`. Only root can open it.

If none of these apply, the command stops with:

```console
$ orch nodes
no target is selected; run 'orch target add NAME ssh://root@CONTROLLER' or 'orch target use NAME'
```

`--target` may appear anywhere before a `--` separator and only once. Pass it
explicitly in scripts and CI so a command never runs against the wrong cluster:

```console
orch deploy --wait --target production
```

`--context FILE` names an administrator context file directly. It remains for
compatibility and cannot be combined with `--target`. See
[targets and interfaces](targets.md) for how targets are stored and reached.

These are the remote commands: `join-token`, `status`, `events`, `deploy`,
`rollback`, `nodes`, `logs`, `exec`, `remove`, `job`, `secret`, `registry`,
`tui`, `web`, `app status|history|diff|watch`, `cron runs|logs`,
`network configure`, and every `node` subcommand except `node install` and
`node doctor`. All other commands run locally.

### Flags and output

- Flags use Go flag syntax: `--flag value`, `--flag=value`, and the single-dash
  forms `-flag` are all accepted.
- Most commands that take positional arguments also accept flags after them,
  for example `orch logs hello web --tail 50`. Commands that print
  "accepts flags only" take no positional arguments.
- `--json` prints machine-readable JSON on stdout. With `--json`, errors are
  also written as JSON (`{"error": "...", "problems": [...]}`) on stderr.
- Streams such as logs and job output write the workload's stdout to stdout and
  its stderr to stderr.

### Idempotency keys

Commands that change cluster state send an idempotency key so a retry after a
lost reply does not apply the change twice. orch generates a key when you omit
`--idempotency-key`. `deploy`, `remove`, `rollback`, `network configure`, and
`job run|retry` print the key they used, and the `node` update commands include
it in their error message, so you can retry with the same value:

```console
orch deploy --idempotency-key idem_4f2a9c0d7e1b3a5c6d8e0f1a2b3c4d5e --target production
```

Keys for `deploy`, `remove`, `network configure`, `node`, and `rollback` must
be 16 to 128 characters: letters, digits, `.`, `_`, `:`, and `-`, starting with
a letter or digit.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | The command succeeded |
| `1` | Operational failure, such as an unreachable controller or a failed rollout |
| `2` | Usage error or manifest validation failure |

## Cluster setup

### orch init

```text
orch init [--name NAME] [--advertise-address IP] [--port 9443] [--recovery-dir DIR] [--wait-timeout 2m] [--json]
```

Installs a controller on this host as a systemd service and waits until it
answers. Run it as root on the controller host.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--name` | `orch` | Cluster name |
| `--advertise-address` | detected | Address workers and clients use to reach this host |
| `--port` | `9443` | Controller port, 1024 to 65535 |
| `--recovery-dir` | none | Also write a day-zero recovery set to this new directory |
| `--wait-timeout` | `2m` | Maximum wait for controller readiness |
| `--json` | `false` | Emit machine-readable JSON |

The controller listens on `0.0.0.0:PORT` and advertises
`https://ADDRESS:PORT`. A break-glass administrator context is written to
`/etc/orch/admin`. See [installation](installation.md#controller).

### orch join-token

```text
orch join-token NODE [--ttl 1h] [--target NAME] [--json]
```

Creates a single-use join token for a worker called `NODE` and prints the
`orch join` command to run on it.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--ttl` | `1h` | Token lifetime, from `1m` through `24h` |
| `--json` | `false` | Print the token id, node name, expiry, token, and command as JSON |

### orch join

```text
orch join TOKEN|- [--advertise-address IP] [--wait-timeout 5m] [--json]
```

Installs a worker on this host with a token from `orch join-token`. Run it as
root on the worker. Pass `-` instead of the token to read it from stdin, which
keeps it out of the process list.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--advertise-address` | detected | IPv4 address other nodes use to reach this host |
| `--wait-timeout` | `5m` | Maximum wait for agent readiness |
| `--json` | `false` | Emit machine-readable JSON |

```console
sudo orch join - < worker-1.token
```

### orch cluster doctor

```text
orch cluster doctor --server HTTPS_URL --listen ADDRESS [--json]
```

Checks whether this host can run a controller and prints each check with its
fix. It exits with `1` when the host is not ready.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--server` | required | Canonical controller HTTPS URL |
| `--listen` | required | Controller IP listen address |
| `--json` | `false` | Emit machine-readable JSON |

### orch cluster install

```text
orch cluster install --name NAME --server HTTPS_URL --listen ADDRESS --admin-dir DIR [--recovery-dir DIR] [--dry-run] [--wait-timeout 2m] [--json]
```

Installs a controller like `orch init`, but lets you choose the URL, listen
address, and where the administrator directory is written.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--name` | required | Cluster name |
| `--server` | required | Canonical controller HTTPS URL |
| `--listen` | required | Controller IP listen address |
| `--admin-dir` | required | New portable administrator directory |
| `--recovery-dir` | none | Optional new day-zero recovery directory |
| `--dry-run` | `false` | Inspect and show planned actions without modifying the host |
| `--wait-timeout` | `2m` | Maximum wait for authenticated controller readiness |
| `--json` | `false` | Emit machine-readable JSON |

Import the resulting `context.json` with `orch target import`.

### orch cluster init

```text
orch cluster init --name NAME --server HTTPS_URL --data-dir DIR [--context FILE] [--json]
```

Creates controller state in a directory without installing services. Use it
when you run `orch controller serve` yourself.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--name` | required | Cluster name |
| `--server` | required | Controller HTTPS URL |
| `--data-dir` | required | Controller state directory |
| `--context` | none | Write the administrator context to this path |
| `--json` | `false` | Emit machine-readable JSON |

Without `--context`, orch stores the administrator credentials as a local
target named after the cluster and selects it.

### orch node doctor

```text
orch node doctor --advertise-address IPV4 [--json]
```

Checks whether this host can run a worker, without changing it. It exits with
`1` when the host is not ready. It always runs locally.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--advertise-address` | required | Private or public IPv4 address of this host that other nodes can reach |
| `--json` | `false` | Emit machine-readable results |

### orch node install

```text
orch node install --advertise-address IPV4 [--join-file FILE|-] [--dry-run] [--wait-timeout 5m] [--json]
```

Installs a worker from a join file created by `orch node join create`. It
always runs locally.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--advertise-address` | required | Private or public IPv4 address of this host that other nodes can reach |
| `--join-file` | none | Private join file path, or `-` for stdin |
| `--dry-run` | `false` | Show planned actions without modifying the host or reading join credentials |
| `--wait-timeout` | `5m` | Maximum wait for authenticated agent readiness |
| `--json` | `false` | Emit machine-readable results |

### orch node join

```text
orch node join create NODE --output FILE [--ttl 15m] [--idempotency-key KEY] [--target NAME] [--json]
orch node join list [--target NAME] [--json]
orch node join revoke JOIN_ID [--idempotency-key KEY] [--target NAME] [--json]
```

Manages join credentials. `create` writes a join file (mode 0600) for
`orch node install`; `list` shows join credentials and their state; `revoke`
invalidates one, including tokens from `orch join-token`.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--output` | required for `create` | New join file path; must not exist |
| `--ttl` | `15m` | Join lifetime, from `1m` through `24h` |
| `--idempotency-key` | generated | Stable key for safe retries |
| `--json` | `false` | Emit machine-readable JSON |

If `create` is interrupted, a private `FILE.pending` stage remains. Rerun the
same command with the same output path to finish it.

## Targets

See [targets and interfaces](targets.md) for a full walkthrough.

### orch target add

```text
orch target add NAME ssh://[USER@]HOST[:PORT] [--use] [--json]
```

Remembers a controller reached over SSH. orch connects once to confirm a
controller answers and records the cluster's identity. Your SSH login is the
credential; no certificate is stored.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--use` | `false` | Select the new target. It is selected automatically when no target is selected yet |
| `--json` | `false` | Emit machine-readable JSON |

The location can also be `unix:///PATH` for an administrator socket on this
machine, and an SSH location can end in a socket path
(`ssh://root@HOST/path/to/admin.sock`). Without a path, the target uses
`/var/lib/orch-controller/admin.sock`.

### orch target import

```text
orch target import NAME CONTEXT [--use] [--replace] [--json]
```

Copies an mTLS administrator context, such as `/etc/orch/admin/context.json`,
into a private target.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--use` | `false` | Select the imported target |
| `--replace` | `false` | Replace the credentials of an existing target for the same cluster and keep its transport |
| `--json` | `false` | Emit machine-readable JSON |

### orch target list

```text
orch target list [--json]
```

Lists remembered targets. The selected one is marked with `*`.

### orch target use

```text
orch target use NAME
```

Selects the target that remote commands use by default.

### orch target transport

```text
orch target transport NAME direct
orch target transport NAME wireguard --address HOST:PORT
orch target transport NAME ssh --host SSH_ALIAS [--destination HOST:PORT]
```

Changes how an imported (certificate) target reaches its controller.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--address` | required for `wireguard` | WireGuard-reachable controller `host:port` |
| `--host` | required for `ssh` | OpenSSH host or alias |
| `--destination` | controller URL host and port | Controller `host:port` as seen from the SSH host |

A target added with `orch target add` cannot switch to a certificate transport,
and an imported target cannot switch to a socket transport. Add a new target
instead.

### orch target test

```text
orch target test [NAME] [--json]
```

Connects to the named or selected target within 10 seconds and prints the
cluster name.

```console
$ orch target test production
Connected to production through target production (socket)
```

## Applications and deployments

These commands read `orch.pkl` in the current directory unless you pass
`-f`. See [applications](configuration.md) for the file format.

### orch validate

```text
orch validate [-f orch.pkl] [--root DIR] [--json]
```

Evaluates and validates the application file and prints its name, service and
cron job counts, and digest. Validation problems exit with `2`.

| Flag | Default | Purpose |
| --- | --- | --- |
| `-f`, `--file` | `orch.pkl` | Pkl manifest path |
| `--root` | manifest directory | Pkl evaluation root |
| `--json` | `false` | Emit machine-readable JSON |

### orch render

```text
orch render [-f orch.pkl] [--root DIR]
```

Prints the canonical JSON that orch would submit for the application.

| Flag | Default | Purpose |
| --- | --- | --- |
| `-f`, `--file` | `orch.pkl` | Pkl manifest path |
| `--root` | manifest directory | Pkl evaluation root |

### orch plan

```text
orch plan [-f orch.pkl] [--root DIR] [--against previous.pkl] [--json]
```

Lists the changes in the application file and summarizes how many items are
added, changed, and removed. It runs locally: without `--against` it compares
with an empty application.

| Flag | Default | Purpose |
| --- | --- | --- |
| `-f`, `--file` | `orch.pkl` | Target Pkl manifest path |
| `--root` | each manifest's directory | Pkl evaluation root |
| `--against` | none | Existing Pkl manifest to compare against; must have the same application name |
| `--json` | `false` | Emit machine-readable JSON |

To compare against what is deployed, use `orch app diff`.

### orch deploy

```text
orch deploy [-f orch.pkl] [--root DIR] [--wait] [--timeout 10m] [--idempotency-key KEY] [--target NAME] [--json]
```

Submits the application as desired state. Without `--wait` it returns as soon
as the controller accepts it; the rollout continues in the background.

| Flag | Default | Purpose |
| --- | --- | --- |
| `-f`, `--file` | `orch.pkl` | Pkl manifest path |
| `--root` | manifest directory | Pkl evaluation root |
| `--wait` | `false` | Wait for this revision to become ready, printing progress |
| `--timeout` | `10m` | Rollout wait deadline |
| `--idempotency-key` | generated | Stable key for safe retries |
| `--json` | `false` | Emit machine-readable JSON |

Submitting the same configuration again reports that the desired state is
already current. With `--wait`, the result is `READY`, `FAILED`, `TIMED_OUT`,
or `CANCELED`. Stopping the watcher does not cancel the deployment; resume
watching with `orch app watch`.

### orch app status

```text
orch app status APP [--target NAME] [--json]
```

Shows the desired, observed, and active generation of an application, its
status and reason, every replica with its health and node, HTTP and TCP routes,
node ports, and cron jobs with their next run.

### orch app history

```text
orch app history APP [--limit 20] [--before-generation N] [--target NAME] [--json]
```

Lists deployments, newest first, with revision, outcome, and resolved images.
The current and active deployments are marked.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--limit` | `20` | Page size, 1 to 50 |
| `--before-generation` | none | Show deployments older than this generation |
| `--json` | `false` | Emit machine-readable JSON |

When more history exists, the output ends with the command for the next page.

### orch app diff

```text
orch app diff APP REVISION [--against REVISION] [--target NAME] [--json]
```

Compares a stored revision with the current desired state, or with another
revision. Output is limited to 500 changes.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--against` | current desired state | Revision to compare to |
| `--json` | `false` | Emit machine-readable JSON |

### orch app watch

```text
orch app watch APP [--revision REVISION] [--timeout 10m] [--target NAME] [--json]
```

Follows a rollout until it is ready, fails, or the deadline passes.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--revision` | current desired state | Revision to watch |
| `--timeout` | `10m` | Rollout wait deadline |
| `--json` | `false` | Emit machine-readable JSON |

### orch rollback

```text
orch rollback APP REVISION [--expected-generation N] [--wait] [--timeout 10m] [--idempotency-key KEY] [--target NAME] [--json]
```

Creates a new deployment from a stored revision, reusing its resolved image
digests. Before sending the request, orch prints the exact command to retry it
safely.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--expected-generation` | current generation | Generation being replaced; the rollback is refused if the application has moved on |
| `--wait` | `false` | Wait for the rollback revision to become ready |
| `--timeout` | `10m` | Rollout wait deadline |
| `--idempotency-key` | generated | Stable retry key; requires `--expected-generation` |
| `--json` | `false` | Emit machine-readable JSON |

See [rollbacks](runtime.md#rollbacks).

### orch remove

```text
orch remove APP [--idempotency-key KEY] [--target NAME] [--json]
```

Removes an application's desired state, which stops its workloads.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--idempotency-key` | generated | Stable key for safe retries |
| `--json` | `false` | Emit machine-readable JSON |

### orch schema

```text
orch schema
```

Prints the embedded `Application.pkl` schema that `orch:/v1alpha1/Application.pkl`
refers to.

### orch pkl reader

```text
orch pkl reader
```

Serves the embedded schema over Pkl's external-reader protocol on stdin and
stdout, so stock Pkl tooling can resolve `orch:` modules. You do not run it by
hand.

## Status, events, and logs

### orch status

```text
orch status [--target NAME] [--json]
```

Shows the cluster name and ID, controller URL, generation, workload network,
creation time, and when the administrator and controller certificates expire.
Certificates that expire within 30 days are flagged with the renewal command.

### orch nodes

```text
orch nodes [--target NAME] [--json]
```

Lists every node with its lifecycle and connection state, operating system,
capacity, allocatable and assigned resources, labels, network and ingress
state, drain blockers, and certificate expiry.

### orch events

```text
orch events [--after SEQUENCE] [--limit 50] [--target NAME] [--json]
```

Prints cluster events with their sequence number, time, kind, and JSON payload.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--after` | `0` | Only events after this sequence number |
| `--limit` | `50` | Maximum events to return, 1 to 200 |
| `--json` | `false` | Emit JSON, including `next_after_sequence` for the next call |

### orch logs

```text
orch logs APP SERVICE [--replica 1] [--tail 100] [-f] [--target NAME]
```

Prints the most recent log lines of one service replica and exits. With `-f`
it keeps printing new output until you press Ctrl-C, and carries on through
restarts and short disconnects, noting them on stderr.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--replica` | `1` | Replica ordinal |
| `--tail` | `100` | Maximum lines per stream, 1 to 10000 |
| `-f`, `--follow` | `false` | Keep printing new output |

### orch restart

```text
orch restart APP [SERVICE] [--replica N] [--target NAME]
```

Restarts a service, or every service of the application, with the same spec.
Replicas restart one at a time, and each must be running again before the next
one stops. Each replica gets its `stopGracePeriod` to shut down. `--replica`
restarts just one replica.

### orch exec

```text
orch exec APP SERVICE [--replica 1] [--timeout 30] [--target NAME] -- COMMAND [ARG...]
```

Runs a non-interactive command inside a running replica and streams its
output. A non-zero exit status from the command makes `orch exec` exit with `1`.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--replica` | `1` | Replica ordinal |
| `--timeout` | `30` | Timeout in seconds, 1 to 300 |

The command takes 1 to 64 non-empty arguments and at most 8 KiB in total.

```console
orch exec hello web --replica 1 -- ./bin/healthcheck --verbose
```

### orch volume

```text
orch volume list [APP] [--target NAME] [--json]
orch volume backups APP VOLUME [--target NAME] [--json]
orch volume backup APP VOLUME [--target NAME] [--json]
orch volume restore APP VOLUME BACKUP [--target NAME] [--json]
orch volume download APP VOLUME BACKUP [--output FILE|-] [--target NAME]
```

`list` shows each local volume, its worker, its backup schedule, the newest
backup, and whether the last scheduled backup failed. `backups` lists every
backup of one volume. `backup` takes one now, running the volume's `before`
command first; it is labeled `-manual` and never pruned.

`restore` stops the service that mounts the volume, replaces the volume with
the backup, and starts the service again. The volume's previous contents are
kept as a `-before-restore` backup, and the command prints how to restore it.

`download` copies a backup archive (`.tar.gz`) to this machine, by default as
`APP-VOLUME-BACKUP.tar.gz`; `--output -` writes it to standard output. See
[volume backups](operations.md#volume-backups).

## Jobs

### orch job

```text
orch job run APP SERVICE [--wait] [--revision REVISION] [--timeout 15m] [--wait-timeout 20m] [--idempotency-key KEY] [--target NAME] [--json] -- COMMAND [ARG...]
orch job list [APP] [--limit 20] [--before ID] [--target NAME] [--json]
orch job show ID [--target NAME] [--json]
orch job logs ID [--tail 100] [--target NAME]
orch job cancel ID [--target NAME] [--json]
orch job retry ID [--wait] [--idempotency-key KEY] [--target NAME] [--json]
orch job wait ID [--timeout 15m] [--target NAME] [--json]
```

Runs and manages one-off jobs. `run` starts a command with the image and
settings of a service in the current application revision. The other actions
list, inspect, read logs of, cancel, retry, or wait for a job.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--wait` | `false` | Wait for the exact run to finish (`run`, `retry`, `cancel`) |
| `--revision` | none | Only run if this is the application's current revision |
| `--timeout` | `15m` | Process timeout for `run`; wait deadline for `wait`. 10s to 24h |
| `--wait-timeout` | `20m` | Maximum time `--wait` waits, 1s to 24h |
| `--limit` | `20` | Maximum jobs for `list`, 1 to 100 |
| `--before` | none | History cursor for `list`; printed as `Next page: --before ID` |
| `--tail` | `100` | Log lines per stream for `logs`, 1 to 10000 |
| `--idempotency-key` | generated | Stable key for `run` or `retry`, printed on stderr |
| `--json` | `false` | Machine-readable output |

```console
orch job run hello web --wait -- ./bin/migrate up
```

See [jobs](jobs.md) for pre-deploy hooks and retry behavior.

### orch cron

```text
orch cron run APP JOB [--wait] [--wait-timeout 20m] [--idempotency-key KEY] [--target NAME] [--json]
orch cron runs APP JOB [--limit 20] [--target NAME] [--json]
orch cron logs APP JOB RUN_ID [--tail 100] [--target NAME]
```

`run` starts the scheduled job once now, with the same image, command,
secrets, and read-only volume mounts as its schedule and on the node that owns
the schedule. Use it to check that a backup job works without waiting for its
next run. The run is a job: follow it with `orch job logs ID`, and with
`--wait` the command exits nonzero unless it succeeds. `runs` lists recent
scheduled runs with their scheduled time, state, and reason; `logs` prints the
logs of one.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--wait` | `false` | Wait for the run started by `run` to finish |
| `--wait-timeout` | `20m` | Maximum time `--wait` waits, 1s to 24h |
| `--limit` | `20` | Maximum runs, 1 to 100 |
| `--tail` | `100` | Maximum lines per stream, 1 to 10000 |
| `--json` | `false` | Emit machine-readable JSON (`runs` only) |

## Secrets and registries

### orch secret

```text
orch secret set APP NAME --from-stdin [--idempotency-key KEY] [--target NAME] [--json]
orch secret list APP [--target NAME] [--json]
orch secret delete APP NAME [--idempotency-key KEY] [--target NAME] [--json]
```

Stores, lists, and deletes an application's secrets. `set` only reads the value
from stdin, never from an argument, and accepts 1 byte to 1 MiB. `list` shows
each name, its current version, whether the application references it, and
when it was updated; it never shows values.

```console
printf '%s' "$DATABASE_URL" | orch secret set hello database-url --from-stdin
```

See [secrets](secrets.md).

### orch registry

```text
orch registry login HOST --username USER --password-stdin [--idempotency-key KEY] [--target NAME] [--json]
orch registry list [--target NAME] [--json]
orch registry logout HOST [--idempotency-key KEY] [--target NAME] [--json]
```

Stores, lists, and removes credentials for private image registries. The
password or token is read from stdin and accepts 1 byte to 1 MiB.

## Nodes

### orch node label and unlabel

```text
orch node label NODE KEY=VALUE... [--target NAME] [--json]
orch node unlabel NODE KEY... [--target NAME] [--json]
```

Adds or removes node labels used by service `constraints`. The `orch.*` prefix
is reserved.

### orch node cordon, uncordon, and drain

```text
orch node cordon NODE [--target NAME] [--json]
orch node uncordon NODE [--target NAME] [--json]
orch node drain NODE [--target NAME] [--json]
```

`cordon` stops new placement on a node, `drain` moves its movable workloads
away, and `uncordon` makes it active again. See
[node maintenance](operations.md#node-maintenance).

### orch node ingress

```text
orch node ingress NODE --enable|--disable [--target NAME] [--json]
```

Turns public HTTP, HTTPS, and TCP ingress on or off for a node. Exactly one of
`--enable` or `--disable` is required.

### orch node remove

```text
orch node remove NODE [--lost --confirm-fenced] [--target NAME] [--json]
```

Removes a node from the cluster. For a node that is unreachable, pass both
`--lost` and `--confirm-fenced` to confirm it can no longer run workloads.

All `node` update commands also accept `--idempotency-key KEY`.

## Network

### orch network configure

```text
orch network configure --cidr CIDR [--idempotency-key KEY] [--target NAME] [--json]
```

Sets the private workload network.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--cidr` | `10.88.0.0/16` | RFC 1918 workload CIDR, `/16` through `/19` |
| `--idempotency-key` | generated | Stable key for safe retries |
| `--json` | `false` | Emit machine-readable JSON |

Once a worker reports its network as ready, the network is frozen and a
different CIDR is refused. Choose it before workers join. See
[networking](networking.md).

## Upgrades

### orch cluster upgrade and orch node upgrade

```text
orch cluster upgrade --binary FILE [--dry-run] [--wait-timeout 5m] [--json]
orch node upgrade --binary FILE [--dry-run] [--wait-timeout 5m] [--json]
```

Replaces the installed `orch` binary on a controller or worker with a trusted
candidate and restarts its services. Run each as root on the host it upgrades,
controller first.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--binary` | required | Trusted local candidate `orch` executable |
| `--dry-run` | `false` | Validate and show the upgrade without changing the host |
| `--wait-timeout` | `5m` | Maximum wait for service readiness |
| `--json` | `false` | Emit machine-readable JSON |

The output reports compatibility, the candidate version and SHA-256, planned
actions, and a recovery path. See [upgrades](operations.md#upgrades).

## Backup and recovery

These commands work on controller files directly and run as root on the
controller host or a recovery host.

### orch cluster backup

```text
orch cluster backup --data-dir DIR --output FILE [--secret-key-output FILE] [--json]
```

Writes a standalone copy of the controller database.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--data-dir` | required | Controller state directory, `/var/lib/orch-controller` for installed controllers |
| `--output` | required | New backup database path |
| `--secret-key-output` | none | New path for the secret-encryption key, when one is required |
| `--json` | `false` | Emit machine-readable JSON |

### orch cluster backup inspect

```text
orch cluster backup inspect --backup FILE [--pki-dir DIR] [--secret-key FILE] [--json]
```

Checks a backup without restoring it and reports its cluster, schema version,
counts of nodes, applications, deployments, and jobs, and any missing recovery
material.

### orch cluster restore

```text
orch cluster restore --backup FILE|DIR [--pki-dir DIR] [--secret-key FILE] --data-dir NEW_DIR [--json]
```

Restores a backup into a new controller directory, which must not exist.
`--backup` also takes one of the controller's automatic backup directories
from `DATA_DIR/backups`, which already holds the PKI and secret key, so the
other two flags are not needed. `orch cluster backup inspect` accepts the
same.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--backup` | required | Standalone database backup, or an automatic backup directory |
| `--pki-dir` | none | Saved controller PKI directory |
| `--secret-key` | none | Saved secret-encryption key |
| `--data-dir` | required | New controller directory |
| `--json` | `false` | Emit machine-readable JSON |

### orch cluster certificates renew

```text
orch cluster certificates renew --data-dir DIR --admin-dir NEW_DIR [--json]
```

Issues new controller and administrator certificates from the existing cluster
CA. Restart the controller afterwards and import the new context with
`orch target import NAME CONTEXT --replace`. See
[certificate renewal](security.md#certificate-renewal).

## TUI and web dashboard

### orch tui

```text
orch tui [--target NAME] [--refresh 2s]
```

Opens the keyboard-driven terminal interface for the selected target. It needs
an interactive terminal.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--refresh` | `2s` | Refresh interval, `1s` through `1m` |

### orch web

```text
orch web [--target NAME] [--port N] [--no-open]
```

Serves the browser dashboard for the selected target on `127.0.0.1` and prints
a one-time URL. Press Ctrl+C to stop it.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--port` | `0` | Loopback port; `0` picks a free port |
| `--no-open` | `false` | Print the URL without opening a browser |

See [targets and interfaces](targets.md) for both interfaces.

## Services and internals

These commands are run by the systemd units that `orch init`, `orch join`, and
the install commands create. You rarely run them yourself.

### orch controller serve

```text
orch controller serve --data-dir DIR --listen ADDRESS [--admin-socket PATH|off] [--backups-keep 7]
```

Runs the controller process. It writes a backup into `DATA_DIR/backups` once a
day; see [backups](operations.md#backups).

| Flag | Default | Purpose |
| --- | --- | --- |
| `--data-dir` | required | Initialized controller state directory |
| `--listen` | required | TCP listen address, for example `0.0.0.0:9443` |
| `--admin-socket` | `DATA_DIR/admin.sock` | Local administrator socket; `off` disables it |
| `--backups-keep` | `7` | Daily backups to keep, 0 to 365; `0` turns automatic backups off |

### orch agent init

```text
orch agent init --join-file FILE|- --data-dir DIR [--json]
```

Enrolls an agent with a join file and writes its state directory.

### orch agent serve

```text
orch agent serve --data-dir DIR --containerd-socket PATH --advertise-address IPV4 [flags]
```

Runs the worker agent.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--data-dir` | required | Agent state directory |
| `--containerd-socket` | required | containerd socket path |
| `--advertise-address` | required | Private or public IPv4 address reachable by the other nodes |
| `--snapshotter` | `overlayfs` | containerd snapshotter |
| `--wireguard-port` | `51820` | WireGuard UDP listen port |
| `--system-reserved-cpu` | `500m` | CPU reserved for the host |
| `--system-reserved-memory` | `512Mi` | Memory reserved for the host |
| `--cni-bin-dir` | `/opt/cni/bin` | Directory containing the CNI plugins |
| `--caddy-binary` | `/usr/bin/caddy` | Pinned Caddy executable |
| `--ingress-user` | `orch-ingress` | Unprivileged user that runs Caddy |
| `--ingress-data-dir` | `/var/lib/orch-ingress` | Private Caddy data directory owned by `--ingress-user` |

### Other commands

| Command | Purpose |
| --- | --- |
| `orch version [--json]` | Print the version, or build information as JSON |
| `orch help` | Print the usage summary |
| `orch runtime-log-sink`, `orch runtime-secret-init` | Helpers that the agent starts inside workload runtimes; not for direct use |
