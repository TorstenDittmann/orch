# Cluster operations

orch runs one durable controller and one through 20 token-enrolled
execution agents. Agents use an externally installed containerd/runc runtime.
Controller HA, automatic host fencing, and CA/controller/administrator
certificate rotation are not included. Controller snapshots can be inspected and restored
into a fresh directory using separately saved identity and key material.

## Initialize a cluster

Choose a location for controller state. The CLI creates a private, remembered
local target with the cluster name and selects it automatically.

~~~console
orch cluster init \
  --name lab \
  --server https://controller.example.com:9443 \
  --data-dir /var/lib/orch
~~~

The name is a lowercase DNS label. The server must be an HTTPS origin without
a path, query, or fragment. The data and context paths may not overlap.

The command creates this layout:

~~~text
/var/lib/orch/
├── control.db
└── pki/
    ├── ca.pem
    ├── ca-key.pem
    ├── controller.pem
    └── controller-key.pem

~/.config/orch/
├── current.json
└── targets/lab/
    ├── target.json
    ├── context.json
    └── identities/<cluster-id>/
        ├── ca.pem
        ├── admin.pem
        └── admin-key.pem
~~~

Target files are versioned and private. Running initialization with an explicit
`--context FILE` retains the old portable-context workflow for automation.
Existing clusters can migrate once with `orch target import NAME CONTEXT
--use`; the imported target owns private copies, so routine commands no longer
depend on the original path.

Back up the whole platform configuration directory as administrator material,
not individual files from inside a target. If the selected target is broken,
select a known-good one with `orch target use NAME`, use `ORCH_TARGET=NAME` for
an immediate override, or import the recovered context under a new name. Orch
never prints credential contents while listing or testing targets.

## Connect from a workstation

All remote commands and `orch tui` use the current target by default. Select a
different remembered target with `orch target use NAME`, override one command
with `--target NAME`, or set `ORCH_TARGET` for a shell or script.

~~~console
orch target list
orch target use production
orch target test
orch tui
~~~

Direct transport uses the controller URL in the imported context. WireGuard
transport changes only the dial address and expects the operating system to
already own the interface and route:

~~~console
orch target transport production wireguard --address 10.20.0.10:9443
~~~

SSH transport uses the system OpenSSH client and an existing host alias. Make
sure `ssh prod-controller` already succeeds with the intended host-key policy,
agent, key, and optional ProxyJump, then configure and test it:

~~~console
orch target transport production ssh --host prod-controller
orch target test production
~~~

The SSH forward listens only on loopback and exits with the command or TUI.
Both WireGuard and SSH preserve the canonical controller hostname for TLS and
the same administrator mTLS identity. Orch does not provision workstation
WireGuard interfaces, manage SSH keys, weaken host-key checks, or copy an
administrator key from a server.

Inside the TUI, `ctrl+p` opens the command palette and fuzzy resource switcher,
`tab` changes view, `?` shows contextual help, and `q` exits. In the event and
runtime views, `/` filters the focused pane locally. The runtime workspace lists
all current service replicas and retained cron runs without preloading
application detail. Use `g` to group by application or node, Enter to open the
exact selected runtime's logs, `h/l` to move between the tree and logs, `s` to
select stdout/stderr/all, and `G` to resume live output after scrolling. Node
cordon, uncordon, drain, and ingress changes require confirmation. The TUI
keeps the last good snapshot visibly marked degraded during connection loss.

The first controller start also creates `/var/lib/orch/controller.lock`. The
file remains as harmless ownership metadata after a clean stop; the advisory
lock, not the file's presence, determines whether a controller is active.

## Start and stop the controller

The listen address is independent from the advertised HTTPS origin. They are
normally the same host and port; keeping them separate also permits a future
front proxy without changing durable cluster identity.

~~~console
orch controller serve \
  --data-dir /var/lib/orch \
  --listen 0.0.0.0:9443
~~~

Startup validates the database, migration history, private file permissions,
cluster CA, controller certificate/key, cluster identity, and advertised
hostname before opening the API. `controller.lock` combines an operating-system
advisory lock with in-process ownership. A second controller for the same data
directory exits without opening another listener.

SIGINT and SIGTERM trigger a bounded graceful HTTP shutdown, SQLite WAL
checkpoint, and lock release. A subsequent process can reopen the same desired
state and idempotency history.

Stopping or losing the controller does not stop an already assigned workload.
The agent follows only its last durably cached full assignment, continues local
restart supervision, and converges with the controller after reconnecting.

`GET /healthz` is available over HTTPS without a client certificate for local
probes. It returns only `ok`, never cluster metadata. All ConnectRPC operator
methods require a verified administrator certificate.

## Upgrade an existing one-node cluster

Plan #9 changes the internal controller/agent protocol, so upgrade the
controller and agents as one coordinated maintenance operation. Stop the
existing agent, stop and back up the controller, upgrade both binaries, start
the controller so its forward-only migration completes, and then start the
original agent before enrolling more nodes. Do not run old and new protocol
versions together.

The migration preserves the original node ID, certificate and public-key
binding, first workload `/24`, immutable application/assignment history,
encrypted records, and local runtime ownership labels. Matching existing
workloads are adopted rather than recreated. A failed migration must be
recovered from the pre-upgrade controller backup; do not edit SQLite or
host-local CNI state by hand.

## Configure the workload CIDR

New clusters use `10.88.0.0/16`. Before the first agent reports a ready
workload network, an administrator may select another canonical RFC1918 prefix
from `/16` through `/19`:

~~~console
orch network configure \
  --cidr 10.96.0.0/16
~~~

The command is idempotent and reports its retry key. The first `READY` network
observation freezes the CIDR permanently for this cluster; later attempts to
change it fail closed. Every active node owns one stable `/24` and uses its
first usable address as the bridge and DNS gateway. A migrated first node keeps
the first `/24` and existing workload objects.

## Enroll and run the agent

On an administrator machine, create an expiring single-use artifact bound to
one exact node name:

~~~console
orch node join create node-1 \
  --ttl 15m \
  --output ./node-1.join
~~~

The artifact expires after 15 minutes by default; `--ttl` accepts one minute
through 24 hours. It is a bearer credential until consumed or expired. The CLI
creates it as a new mode-`0600` file and refuses overwrite. Transfer it over a
protected channel, do not place its contents in arguments or logs, and revoke
it promptly after suspected disclosure:

~~~console
orch node join list
orch node join revoke JOIN_ID
~~~

On the worker, either read that private regular file or stream it over stdin:

~~~console
orch agent init \
  --join-file ./node-1.join \
  --data-dir /var/lib/orch-agent

# Equivalent streaming form:
orch agent init --join-file - --data-dir /var/lib/orch-agent < ./node-1.join
~~~

`agent init` generates a P-256 private key locally, durably stages the exact CSR
and retry key, pins bootstrap TLS to the artifact's CA, endpoint, cluster, and
controller identity, and sends no administrator credential to the worker. The
controller stores only a SHA-256 hash of the random token secret. If the request
outcome is uncertain, rerun the exact command with the same data directory and
artifact; orch resumes the staged request and receives the frozen response. A
changed CSR, name, token, or retry key is rejected. At most 20 active nodes may
be enrolled and at most 100 unexpired unused artifacts may exist.

The original artifact is no longer needed after successful initialization; the
completed data directory is self-contained. Repeat the create/init flow for
each host with a unique node name and private data directory.

Start the agent on a supported Linux host:

Install the official CNI plugins 1.9.1 in `/opt/cni/bin`, enable
`net.ipv4.ip_forward=1`, and make nftables available. On ingress-enabled nodes,
install Caddy 2.11.3 as a root-owned executable with only the low-port
capability, then prepare its dedicated identity and private state directory.
Exact account-management commands vary by distribution; the resulting
invariants can be checked with:

~~~console
setcap cap_net_bind_service=+ep /usr/bin/caddy
install -d -m 0700 -o orch-ingress -g orch-ingress /var/lib/orch-ingress
~~~

~~~console
orch agent serve \
  --data-dir /var/lib/orch-agent \
  --advertise-address 10.0.0.11 \
  --wireguard-port 51820 \
  --system-reserved-cpu 500m \
  --system-reserved-memory 512Mi \
  --containerd-socket /run/containerd/containerd.sock \
  --snapshotter overlayfs \
  --cni-bin-dir /opt/cni/bin \
  --caddy-binary /usr/bin/caddy \
  --ingress-user orch-ingress \
  --ingress-data-dir /var/lib/orch-ingress
~~~

The advertised address must be a canonical private IPv4 address reachable
from every other node. Permit the
configured UDP port, 51820 by default, between nodes. Reservations are
subtracted from detected host totals; remaining CPU and memory are hard
scheduling capacity, with no overcommit.

The process validates the protected identity and SQLite cache before contacting
containerd. It then requires Linux `amd64` or `arm64`, cgroups v2, containerd
2.3.4, the configured snapshotter, `containerd-shim-runc-v2`, `runc`, official
CNI plugins 1.9.1 with CNI 1.1.0 support, IPv4 forwarding, nftables, and the
WireGuard kernel support and an available UDP listen port. An ingress-enabled
node additionally requires the exact Caddy version, file capability, dedicated
user, and owned admin socket; compute-only nodes do not. DNS and the requested
network/ingress role must be ready before the first network generation is
accepted. Missing capabilities are reported to the controller as explicit
blockers. Run only one agent process for the data directory; `agent.lock`
enforces local ownership.

Agent certificates last one year and renew automatically when they enter the
final 90 days, with stable per-node jitter. Renewal reuses the protected agent
key and node ID. The controller first records a pending certificate, and only
activates it after the agent publishes it and reconnects successfully.
`orch nodes` reports certificate expiry and `HEALTHY`, `RENEWAL_DUE`,
`RENEWAL_PENDING`, or `RENEWAL_FAILED`. Renewal retries use bounded backoff and
do not stop cached workload, DNS, ingress, WireGuard, cron, or restart
supervision.

Automatic recovery after certificate expiry is deliberately unsupported. An
already-running agent continues its cached local work, but a stopped or
disconnected expired agent cannot authenticate. Recovering that identity needs
a future key-recovery design; do not weaken TLS or copy the cluster CA key to a
worker.

See [networking.md](networking.md) for CIDR conflict recovery, DNS behavior,
firewall ownership, public DNS and port requirements, Caddy persistence, and
the endpoint drain/removal contract.

The resulting layout is:

~~~text
/var/lib/orch-agent/
├── agent.json
├── agent.db
├── agent.lock
├── pki/
│   ├── ca.pem
│   ├── agent.pem
│   └── agent-key.pem
├── logs/
├── network/
│   ├── cni-cache/
│   ├── host-local/
│   ├── firewall-owner.json
│   ├── resolv.conf
│   ├── resolv-<application>.conf
│   └── wireguard/
│       ├── owner.json
│       └── wireguard.key
└── volumes/
~~~

Stop the agent with SIGINT or SIGTERM. It closes its control stream and cache
but intentionally leaves desired tasks running. On restart, matching
orch-labeled tasks are adopted; mismatched objects are refused.

## Label and operate nodes

The node-management commands are:

~~~console
orch node label node-1 region=eu-west storage=fast
orch node unlabel node-1 storage
orch node cordon node-1
orch node uncordon node-1
orch node ingress node-1 --enable
orch node drain node-1
orch node remove node-1
orch node remove node-1 --lost --confirm-fenced
~~~

Labels are bounded exact key/value placement data. The `orch.*` prefix is
reserved for immutable system labels. Changing a label may move only affected
stateless allocations; it does not trigger a general rebalance. Ingress is a
dedicated node property, not a label.

Lifecycle and connection are separate:

- `ACTIVE` nodes may keep and receive work.
- `CORDONED` nodes keep current work but receive no new placements.
- `DRAINING` nodes receive no new work and progressively evacuate movable work
  within rollout budgets. The operation resumes after controller restart.
- `REMOVED` nodes no longer authenticate, receive assignments, or join the
  WireGuard mesh.

Graceful drain waits for active cron runs and old assignment acknowledgement.
A durable local-volume claim cannot move, so drain reports
`local_volume_pinned` until the operator chooses how to handle that data.
Normal removal requires the node connected, drained, empty, and acknowledging
its final empty assignment.

For an unreachable host, first fence it outside orch by powering it off or
isolating its network/storage. Only then use `--lost --confirm-fenced`. This
assertion allows its subnet to be reused and its cron templates to be assigned
elsewhere; it never copies or recreates local-volume data. Without confirmed
fencing, an unreachable cron owner remains sticky to avoid an automatic
duplicate run.

## Inspect and submit desired state

Every remote CLI command uses the selected target and mTLS:

~~~console
orch status

orch deploy \
  --file orch.pkl

orch events \
  --after 0 \
  --limit 50
~~~

The CLI evaluates the typed Pkl module on the operator machine, restricts local
imports to the configured root, permits HTTPS module imports, applies defaults
and validation, and sends its exact canonical JSON. The controller is
authoritative for strict canonical decoding, Go validation, canonical-byte
verification, and the SHA-256 digest; it never runs Pkl. A changed apply
atomically commits the application generation, immutable revision, ordered
`application.desired.updated` event, and idempotency response. Upgrade CLI and
controller together when moving from the former TOML transport.

`orch deploy` generates and reports a random idempotency key by default. For
automation or an explicit retry, supply a stable 16–128 character key with
`--idempotency-key`. Reusing a key with the same canonical manifest returns the
original response. Reusing it for different desired state is a conflict. A new
key is a no-op only when both authored and resolved canonical state remain
current: it creates neither a generation nor an event.

For this single-administrator MVP, idempotency records are retained for the
life of the cluster. A future expiration or compaction policy must preserve a
clearly versioned replay window.

The deploy success response is intentionally an acceptance receipt rather than
a race-dependent observation:

~~~text
Status: PENDING — accepted for asynchronous execution; use orch app status to observe it.
~~~

Use `--json` for stable machine-readable status, event, and deployment output.
Event pagination uses an exclusive sequence cursor and returns the next cursor.

## Deploying image tags

Both services and cron jobs can use `image = "ghcr.io/myteam/api:v1.2"`.
Run `orch registry login` for private registries before deploying. Resolution
happens on the controller, so it needs HTTPS access to the registry and its
authentication service. Failed resolution does not create a deployment.

Each new deploy command resolves tags again. To recover an uncertain response,
reuse the original idempotency key and exact manifest; an accepted request
returns its original receipt even if the tag moved or the registry is offline.
A request that was never accepted may resolve to a newer digest on retry.
Do not reuse a key for a different manifest.

The apply receipt's `digest` remains the hash of the submitted canonical
manifest. Application and deployment reads expose the hash of the resolved
manifest, which can differ. `orch app history` identifies the recorded images;
`orch app diff` compares resolved revisions, so moving a tag shows an image
change. Local manifest previews cannot determine a tag's digest without a deploy.

## Deployment history, comparison, and rollback

Use the selected target for these commands, or pass `--target NAME`:

~~~console
orch app history runtime-demo
orch app history runtime-demo --limit 20 --before-generation 21
orch app diff runtime-demo REVISION_ID
orch app diff runtime-demo OLD_REVISION_ID --against NEW_REVISION_ID
orch rollback runtime-demo REVISION_ID --wait --timeout 10m
~~~

History lists immutable revisions newest first, with creation times, image
references, desired/active markers, rollback provenance, and retained outcomes.
For resolved tags, CLI and TUI history show `requested tag -> resolved digest`;
JSON image summaries expose `requested_image` alongside the resolved `image`.
Older records fall back to their stored image reference.
`--before-generation` is exclusive, so adding a deployment does not shift older
pages. The default page is 20 records; the maximum is 50. Response sizes are
bounded and the returned cursor indicates more records.

Outcomes are `IN_PROGRESS`, `SUCCEEDED`, `FAILED`, `ROLLED_BACK`, `SUPERSEDED`,
`REMOVED`, or `UNKNOWN`. The first observed terminal rollout result is retained;
subsequent health changes update the last observation without rewriting that
result. Replacing or removing an unfinished deployment closes it as superseded
or removed. Revisions from before deployment history was introduced have
`UNKNOWN` outcomes unless the current execution observation can establish a
result. History persists across controller restarts and application removal.

Comparison goes **from the selected revision to the current desired revision**,
or to the explicit `--against` revision. It compares resolved configuration,
without reevaluating Pkl. Image, replica, resource, schedule, and deployment-policy
values are displayed. Other values, including commands, arguments, environment,
secret references, and added/removed composite objects, are redacted; their
changed paths remain visible. Comparisons are bounded to 500 changes and 1 MiB,
with an explicit truncation flag. History and comparison never fetch secret
contents.

Manual rollback restores an older revision's stored configuration as a **new
revision and generation**, following ordinary placement and health gates. It
records its source revision and preserves history. Resolved image digests and
the original requested references are copied without contacting the registry or
resolving tags again. It does not restore volume
contents, database state, or past secret values: secret references resolve
through the current secret store. Removed applications cannot be reactivated
through rollback. Missing resources or secrets can block the new deployment.

Before submitting, the CLI reads the current generation (or uses an explicit
`--expected-generation`). The controller checks that generation transactionally,
so a concurrent deploy or removal cannot be silently overwritten. Rollback
always creates a new revision, including when an older source has the same
configuration digest as current desired state.

For an uncertain response, retry with **both** the printed expected generation
and idempotency key, on the same target:

~~~console
orch rollback runtime-demo REVISION_ID \
  --expected-generation 7 \
  --idempotency-key SAME_KEY
~~~

Replaying that exact request returns the original `PENDING` acceptance receipt,
even after another deployment or removal. Changing its source or expected
generation while reusing the key is a conflict.

### Wait for a rollout

~~~console
orch deploy --file orch.pkl --wait --timeout 10m
orch app watch runtime-demo --timeout 10m
orch app watch runtime-demo --revision REVISION_ID --timeout 10m
~~~

`deploy --wait` and `rollback --wait` watch their exact accepted generation.
`app watch` defaults to the current desired revision. Polling shows execution
state and per-replica progress. Success requires the desired, observed, and
active revision to agree and execution to be `HEALTHY`, or `RUNNING` for
workloads without readiness gates. A healthy older fallback does not count as
success. Cron-only applications become ready when their schedules are
acknowledged and observed; no cron execution is required. Mixed applications
also wait for their desired schedules.

The default deadline is ten minutes. `FAILED`, automatic `ROLLED_BACK`,
supersession, removal, connection failure, cancellation, or timeout returns
exit code 1. `BLOCKED` and `DEGRADED` keep waiting until resolution or the
deadline. Stopping the watcher never cancels an accepted deployment. Use
`app watch --revision` to observe it again.

With `--json`, waiting emits one final object containing application, generation,
revision, status (`READY`, `FAILED`, `TIMED_OUT`, or `CANCELED`), the last execution
status, and any error. Mutating waits also include their idempotency key. The
existing JSON and asynchronous `PENDING` behavior of `deploy` without `--wait`
is unchanged.

### Deployment workspace in the TUI

In Applications, select an application and press `d`, or choose its Deployments
entry through `ctrl+p`. View 7 opens the most recently selected deployment
workspace. Use arrows or `j/k` to select a revision, Enter to compare it with
current desired state, and `b` to prepare a rollback. The confirmation names
the target, application, source revision, and generation being replaced.
`y` submits; `n` or Esc cancels. A changed generation invalidates an initial
confirmation. An uncertain reply retains the same request for a safe retry.

Wide terminals show history and detail side by side. Narrow terminals use
history and detail screens with Esc to return. Detail includes images, rollout
outcome, the latest per-replica progress, and comparison. Enter focuses detail;
arrows scroll it, Home/End jump, `n` opens an older history page, `N` returns to
the newest page, and `r` refreshes. Application and history polling retain the
last good snapshot marked stale on failure, and late responses cannot replace
another selection. All operations use the existing authenticated target client,
including direct, WireGuard, and SSH transports.

## Inspect and operate workloads

~~~console
orch nodes
orch app status runtime-demo

orch logs runtime-demo worker --replica 1 --tail 100

orch exec runtime-demo worker --replica 1 -- /bin/echo ok
~~~

Node lifecycle is durable; connection is the current controller process's
observation. `orch nodes` includes labels, total/reserved/assigned capacity,
subnet, mesh and ingress state, plus drain blockers. An application reports
node name, logical replica, immutable attempt ID, placement reason, desired and
active revisions, and ordered route states. `RUNNING` means the exact desired processes are alive. HTTP
applications reach `HEALTHY` only after readiness and all requested routes are
active; `ROLLED_BACK` identifies the still-active previous generation. See
[runtime.md](runtime.md) for every status and runtime limit.

The non-interactive `orch logs` command remains a finite tail. The TUI builds a
bounded incremental follow session from authenticated continuation reads;
stdout and stderr remain separate and are streamed through the controller
without being persisted there. Rotation, truncation, runtime replacement,
exit, or disconnection are shown explicitly and reset continuation safely.
Exec has no stdin or TTY, is never retried, accepts at most 64 arguments/8 KiB, and has a
maximum five-minute timeout. Both operations resolve the current placement and
require its exact node to be connected. Stale attempt IDs fail closed rather
than reaching an old process.

## Remove an application

~~~console
orch remove runtime-demo
~~~

Removal first tombstones desired state and returns `STOPPING`. The agent stops
and removes owned tasks, containers, writable snapshots, and allocation leases;
after its empty report the application becomes `REMOVED`. Immutable revisions,
events, and directories under `/var/lib/orch-agent/volumes` are retained.

The command prints its idempotency key. Retry an uncertain removal with that
exact key. Repeating removal with a new key is also safe, but the printed key is
the exact replay handle for the original response.

## Create a database backup

The controller database remains open while SQLite's online backup API creates
a consistent snapshot:

~~~console
orch cluster backup \
  --data-dir /var/lib/orch \
  --output /var/backups/orch/lab-2026-09-02.db \
  --secret-key-output /media/offline/orch/lab-2026-09-02.key
~~~

The output must be outside the live data directory and must not already exist.
Before publication, orch runs SQLite integrity and foreign-key checks, syncs
the staged file, and publishes it without clobbering another backup. When the database records a master key (including before its first secret),
`--secret-key-output` is required and is also
created without clobbering. Store that key separately from the database.

The database backup contains cluster state, enrolled-node public metadata,
assignment history, observed execution state, events, and encrypted secret
records. It does not contain the secret decryption key, CA, controller key,
administrator key, agent private identity, agent cache, workload logs, or local
volume contents. Preserve those files separately with an access-controlled
system backup. In particular, save the controller's `pki/` directory containing
`ca.pem`, `ca-key.pem`, `controller.pem`, and `controller-key.pem`. Keep directory
permissions `0700` and file permissions `0600`. Preserve the administrator's
context and identity files on the machine used to operate the cluster.

## Inspect and restore a controller

Inspect a standalone database snapshot without modifying it:

~~~console
orch cluster backup inspect --backup /var/backups/orch/lab.db

orch cluster backup inspect \
  --backup /var/backups/orch/lab.db \
  --pki-dir /media/offline/orch/pki \
  --secret-key /media/offline/orch/lab.key \
  --json
~~~

Inspection checks SQLite integrity, foreign keys and checksummed migration
history. It tests compatible forward migrations on a private temporary copy;
the original snapshot stays unchanged. It reports the cluster ID, original
endpoint, schema versions, counts of nodes/applications/deployments/jobs, and
missing recovery material. It never returns secret values or private keys.
A successful database-only inspection can have `ready: false`: provide the
reported missing files and inspect again before recovery. Invalid supplied
material is an error. `ready: true` verifies the supplied controller identity
and authenticates every encrypted secret and registry credential version,
including retained historical versions. It does not certify that the snapshot
is recent enough for the agents currently running.

Before starting recovery, stop or externally fence the previous controller.
Two controllers restored from the same identity must never run concurrently.
Move the standalone database backup, saved PKI and master key to the replacement
host, then restore:

~~~console
orch cluster restore \
  --backup /var/backups/orch/lab.db \
  --pki-dir /media/offline/orch/pki \
  --secret-key /media/offline/orch/lab.key \
  --data-dir /var/lib/orch-recovered

orch controller serve \
  --data-dir /var/lib/orch-recovered \
  --listen 0.0.0.0:9443
~~~

The destination must not exist, even as an empty directory or symlink. Restore
creates private directories/files, validates all material before publication,
and publishes the database last. An interrupted restore without `control.db`
is incomplete and cannot start a controller; use a fresh destination to retry.
Restore never overwrites a live directory or generates replacement credentials.
Older compatible schemas are migrated in the recovered copy. A newer schema,
changed migration history, corrupt snapshot, wrong master key, mismatched or
expired controller identity, or a database with SQLite sidecars is rejected.
Use `cluster backup` to obtain a standalone snapshot; do not copy a live WAL
database with filesystem tools.

Keep the saved controller endpoint reachable at the same hostname/IP and port
through DNS/routing before starting the controller. Restore preserves that
endpoint, cluster ID and CA; it does not rewrite agent enrollment or operator
targets. Existing administrator credentials remain valid until expiry. Keep
the agents' private identities and caches intact. Jobs completed in the
snapshot retain their outcomes and idempotency receipts, and are not scheduled
again by restoring it. Unfinished jobs remain tied to their recorded owner;
restore does not retry them on another node.

Recovery returns to the snapshot, not the moment the host failed. Deployments,
new enrollments, certificate renewals, secret changes and job results after the
backup may be absent. A newer agent cache can reject stale or conflicting
assignments; restore deliberately preserves that protection. Do not delete an
agent cache or reset its generation to force a rewind: that can discard run
tombstones and repeat commands. If the snapshot predates the agents' accepted
state or certificate rotation, use a newer backup and investigate the mismatch
before resuming mutations. This release does not reconcile divergent histories
or recreate lost local volumes. Run a recovery drill with a recent snapshot and
verify `orch status`, `orch nodes`, application history and job history before
accepting new deployments.

## Failure boundaries

- Schema migrations are forward-only, contiguous, and checksummed. A binary
  refuses changed migration history or a database created by a newer schema.
- Cluster creation and the `cluster.initialized` event commit in one database
  transaction.
- Credential files are staged and synced before no-clobber publication. If a
  normal initialization error occurs before the database transaction commits,
  orch removes only paths it created.
- A machine or process crash can still leave staged or published credentials
  beside an uninitialized database. The next initialization refuses to replace
  them. Inspect and recover or remove that state manually; never assume it is
  safe to mint a replacement administrator.
- SQLite WAL is the single controller's source of truth. There is no control
  plane high availability in this release. While it is unavailable, agents
  preserve tasks, local restarts, WireGuard, DNS, ingress, and owned cron
  schedules, but deployment, placement, drain, and rollout progress stops.
- Controller state changes and idempotency responses commit in one transaction.
  A transport failure may leave the caller uncertain, so retry with the exact
  idempotency key printed or supplied for that request.
- Assignment ACK means the complete snapshot is in the agent's SQLite cache;
  it does not mean a process has started. Execution state changes only from a
  report matching the current acknowledged generation and digest.
- Local volumes are neither replicated nor included in controller backup. A
  lost agent host can therefore lose the only copy of application data. Its
  allocation reports `volume_node_lost` and is never started against an empty
  volume on another node.
- After 60 seconds unreachable, stateless replicas may be replaced with new
  attempt IDs and stale endpoints are withdrawn. A partitioned old agent may
  still run its cached attempt, so temporary duplicates are possible. orch
  does not provide automatic fencing or singleton execution.

## Scheduled jobs

The sticky owner agent evaluates five-field cron schedules in UTC from its
durable cached assignment, including while the controller is unavailable. It
does not replay historical occurrences after downtime, overlap runs, or retry
failures. Graceful drain transfers ownership only after an active run finishes
and the old node acknowledges removal. An unreachable owner is not replaced
until external fencing is explicitly confirmed; exactly-once execution is not
claimed. Use
`orch cron runs APP JOB` for the latest 20 outcomes and
`orch cron logs APP JOB RUN_ID` for a retained run's logs.

## One-off jobs and migration hooks

Run a maintenance command using a deployed service's pinned image and settings:

~~~console
orch job run shop api --wait --timeout 5m --wait-timeout 10m -- ./migrate up
orch job list shop
orch job show JOB_ID --json
orch job logs JOB_ID --tail 200
orch job cancel JOB_ID
orch job retry JOB_ID --idempotency-key maintenance-retry-001 --wait
orch job wait JOB_ID --timeout 10m
~~~

These commands use the selected target and accept `--target` or `--context`.
`job run` prints its idempotency key before sending the request. Reuse that key
and the same command/options after an uncertain response; the original run is
returned even if the application has since changed. `--revision REVISION_ID`
optionally guards creation against a concurrent deployment. An explicit retry
creates a new run using the original image, command and secret versions, records
`retry_of`, and requires the application still to have that revision. Successful
runs cannot be retried; use a new manual run if deliberate repetition is needed.

`--timeout` controls the job process budget (`15m` by default, `10s`–`24h`).
For `run --wait` and `retry --wait`, `--wait-timeout` controls how long the CLI
waits (`20m` by default). A wait timeout or interrupted CLI does not cancel the
job. Successful waits exit 0; unsuccessful runs exit 1 and report the actual
process exit code in text/JSON. Cancellation remains requested until the owning
agent stops the process and reports a terminal result.

History defaults to 20 entries, supports `--limit 1..100`, and uses an exclusive
`--before JOB_ID` cursor. It retains revision, image, node, kind, exit code,
timestamps and retry provenance. Run metadata survives controller restarts.
Logs use the existing agent-local transport and require the owning node to be
reachable. The agent retains runtime/log objects for the newest 100 completed
one-off runs no longer assigned to it; older metadata remains, but logs expire.

At most 32 one-off runs may be active cluster-wide. Placement checks service
constraints and available resources, including existing runs. Once accepted, a
run stays on its owning node; drain or disconnection does not move it or start it
again. Restore an unavailable owner to confirm termination before retrying.
After an agent restart, an uncertain start is inspected and never automatically
repeated. Application removal requests cancellation of its unfinished jobs.

In the TUI, press `8` for Jobs, or `o` from Applications for that application's
runs. Select a run and press Enter for live logs, `c` to request cancellation,
or `x` to retry a finished unsuccessful run. Cancellation and retry require a
confirmation naming the exact run; uncertain retries retain their request key.
Use `n` for older runs and `N` for the latest page.

Pre-deploy hooks appear in the same history with kind `pre_deploy`. Their failure
keeps existing services assigned and causes `deploy --wait` to fail. Retry the
failed hook to resume that revision, or deploy a corrected configuration. An
unacknowledged cancellation of an older hook blocks a newer hook, avoiding
concurrent migrations for the same application. Rollback restores configuration,
not database contents; a rollback configuration with a hook runs that hook again.

This feature uses agent protocol `v1alpha1-workloads-5`. Upgrade controllers and
agents together; older agents are rejected before receiving the new run controls.
Existing processes retain the normal controller-outage behavior during upgrade.
