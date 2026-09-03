# Cluster operations

orch runs one durable controller and one through 20 token-enrolled
execution agents. Agents use an externally installed containerd/runc runtime.
Controller HA, automatic host fencing, CA/controller/administrator certificate
rotation, and automated restore are not included.

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
log views, `/` filters locally. Logs support next/prior matches, stream
selection, wrapping, pause, and snapshot reload; workload allocations and the
latest retained cron run are available through the palette after application
detail is loaded. Node cordon, uncordon, drain, and ingress changes require
confirmation. The TUI keeps the last good snapshot visibly marked degraded
during connection loss.

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
key applied to already-current canonical state is a no-op: it creates neither a
generation nor an event.

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

Logs are a finite tail, not follow mode. Stdout and stderr remain separate and
are streamed through the controller without being persisted there. Exec has no
stdin or TTY, is never retried, accepts at most 64 arguments/8 KiB, and has a
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
the staged file, and publishes it without clobbering another backup. When any
encrypted secret version exists, `--secret-key-output` is required and is also
created without clobbering. Store that key separately from the database.

The database backup contains cluster state, enrolled-node public metadata,
assignment history, observed execution state, events, and encrypted secret
records. It does not contain the secret decryption key, CA, controller key,
administrator key, agent private identity, agent cache, workload logs, or local
volume contents. Preserve those files separately with
an access-controlled system backup. Automated restore over live controller
state is deliberately not implemented yet; stop and wait for a documented
restore workflow rather than copying a database over a live controller.

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
