# Single-node operations

The current implementation runs one durable controller and exactly one
administrator-enrolled execution agent. The agent uses an externally installed
containerd/runc runtime. Multi-node scheduling, node replacement, certificate
rotation, and automated restore are not included.

## Initialize a cluster

Choose separate locations for controller state and the administrator context.
Both should live on storage controlled by the operator. Existing directories
used for private identities must not permit group or world access.

~~~console
orch cluster init \
  --name lab \
  --server https://controller.example.com:9443 \
  --data-dir /var/lib/orch \
  --context "$HOME/.config/orch/context.json"
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
├── context.json
└── identities/<cluster-id>/
    ├── ca.pem
    ├── admin.pem
    └── admin-key.pem
~~~

The context contains relative credential paths, so the context file and its
`identities` directory can be moved together. Running the same command again
validates and returns the existing cluster. It does not rewrite the database,
context, or keys. A changed name or server, or missing/mismatched identity
material, fails closed.

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

## Configure the workload CIDR

New clusters use `10.88.0.0/16`. Before the first agent reports a ready
workload network, an administrator may select another canonical RFC1918 prefix
from `/16` through `/19`:

~~~console
orch network configure \
  --cidr 10.96.0.0/16 \
  --context "$HOME/.config/orch/context.json"
~~~

The command is idempotent and reports its retry key. The first `READY` network
observation freezes the CIDR permanently for this cluster; later attempts to
change it fail closed. The one supported node owns the first `/24` and uses its
first usable address as the bridge and DNS gateway.

## Enroll and run the agent

`agent init` creates a P-256 private key locally, stages its CSR and retry key,
and asks the running controller to enroll the one supported node:

~~~console
orch agent init \
  --name node-1 \
  --context "$HOME/.config/orch/context.json" \
  --data-dir /var/lib/orch-agent
~~~

The controller receives only the signed CSR and returns public certificates.
If the request outcome is uncertain, rerun the exact command with the same data
directory; orch resumes the staged CSR and idempotency key. A second node or a
replacement key for the enrolled node is rejected.

Start the agent on a supported Linux host:

Install the official CNI plugins 1.9.1 in `/opt/cni/bin`, enable
`net.ipv4.ip_forward=1`, and make nftables available. Install Caddy 2.11.3 as a
root-owned executable with only the low-port capability, then prepare its
dedicated identity and private state directory. Exact account-management
commands vary by distribution; the resulting invariants can be checked with:

~~~console
setcap cap_net_bind_service=+ep /usr/bin/caddy
install -d -m 0700 -o orch-ingress -g orch-ingress /var/lib/orch-ingress
~~~

~~~console
orch agent serve \
  --data-dir /var/lib/orch-agent \
  --containerd-socket /run/containerd/containerd.sock \
  --snapshotter overlayfs \
  --cni-bin-dir /opt/cni/bin \
  --caddy-binary /usr/bin/caddy \
  --ingress-user orch-ingress \
  --ingress-data-dir /var/lib/orch-ingress
~~~

The process validates the protected identity and SQLite cache before contacting
containerd. It then requires Linux `amd64` or `arm64`, cgroups v2, containerd
2.3.4, the configured snapshotter, `containerd-shim-runc-v2`, `runc`, official
CNI plugins 1.9.1 with CNI 1.1.0 support, IPv4 forwarding, nftables, and the
exact Caddy version, file capability, dedicated user, and owned admin socket.
DNS and validated Caddy must both be ready before the first network generation
is frozen, even when no public route is declared. Missing network/ingress
capabilities are reported to the controller as explicit blockers. Run only one
agent process for the data directory; `agent.lock` enforces local ownership.

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
│   └── resolv-<application>.conf
└── volumes/
~~~

Stop the agent with SIGINT or SIGTERM. It closes its control stream and cache
but intentionally leaves desired tasks running. On restart, matching
orch-labeled tasks are adopted; mismatched objects are refused.

## Inspect and submit desired state

Every remote CLI command uses the administrator context and mTLS:

~~~console
orch status --context "$HOME/.config/orch/context.json"

orch deploy \
  --file orch.pkl \
  --context "$HOME/.config/orch/context.json"

orch events \
  --context "$HOME/.config/orch/context.json" \
  --after 0 \
  --limit 50
~~~

The CLI evaluates the typed Pkl module locally with a restricted root, applies
defaults and validation, and sends its exact canonical JSON. The controller is
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
orch nodes --context "$HOME/.config/orch/context.json"
orch app status runtime-demo --context "$HOME/.config/orch/context.json"

orch logs runtime-demo worker --replica 1 --tail 100 \
  --context "$HOME/.config/orch/context.json"

orch exec runtime-demo worker --replica 1 \
  --context "$HOME/.config/orch/context.json" -- /bin/echo ok
~~~

Node connection state is the current controller process's observation. An
application reports desired and active generations plus ordered allocation and
route states. `RUNNING` means the exact desired processes are alive. HTTP
applications reach `HEALTHY` only after readiness and all requested routes are
active; `ROLLED_BACK` identifies the still-active previous generation. See
[runtime.md](runtime.md) for every status and runtime limit.

Logs are a finite tail, not follow mode. Stdout and stderr remain separate and
are streamed through the controller without being persisted there. Exec has no
stdin or TTY, is never retried, accepts at most 64 arguments/8 KiB, and has a
maximum five-minute timeout. Both operations require the exact allocation to be
running on a currently connected agent.

## Remove an application

~~~console
orch remove runtime-demo \
  --context "$HOME/.config/orch/context.json"
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
  --output /var/backups/orch/lab-2026-09-02.db
~~~

The output must be outside the live data directory and must not already exist.
Before publication, orch runs SQLite integrity and foreign-key checks, syncs
the staged file, and publishes it without clobbering another backup.

The database backup contains cluster state, enrolled-node public metadata,
assignment history, observed execution state, and events. It does not contain
the CA, controller key, administrator key, agent private identity, agent cache,
workload logs, or local volume contents. Preserve those files separately with
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
  plane high availability in this release.
- Controller state changes and idempotency responses commit in one transaction.
  A transport failure may leave the caller uncertain, so retry with the exact
  idempotency key printed or supplied for that request.
- Assignment ACK means the complete snapshot is in the agent's SQLite cache;
  it does not mean a process has started. Execution state changes only from a
  report matching the current acknowledged generation and digest.
- Local volumes are neither replicated nor included in controller backup. A
  lost agent host can therefore lose the only copy of application data.
