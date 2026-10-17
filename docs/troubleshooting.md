# Troubleshooting

This page is organized by symptom. Find what you are seeing, run the commands
under "Inspect" to confirm the cause, then apply the fix. Most orch errors and
status reasons name the problem directly; the sections below explain what each
one means and what to do about it.

Commands that start with `orch` and no `sudo` run on your workstation against
the selected target. Commands with `sudo` run on the host named in the text.
Add `--target NAME` if you have not selected a target, and `--json` to most
commands for machine-readable output.

## Where to look first

These commands answer most questions:

```console
orch status                      # controller, workload network, certificate expiry
orch nodes                       # every node: connection, capacity, network, DNS, ingress
orch app status APP              # application status, replicas, routes, cron jobs
orch app history APP             # deployments and their outcomes
orch events --limit 50           # recent changes to the cluster
orch logs APP SERVICE --replica 1 --tail 100
```

On a host, the systemd journal has the details that status output summarizes:

```console
sudo journalctl -u orch-controller --since '-10 min'                  # controller host
sudo journalctl -u orch-agent -u orch-containerd --since '-10 min'    # worker
```

And the host checks:

```console
sudo orch node doctor --advertise-address 10.0.0.11
sudo orch cluster doctor --server https://10.0.0.10:9443 --listen 0.0.0.0:9443
```

`orch tui` and `orch web` show the same information live.

### Reading status output

`orch app status` prints one summary and one line per replica, route, and cron
job. The parts that matter most when something is wrong:

| Field | Meaning |
| --- | --- |
| `Status` | Overall state, for example `EXECUTION_STATUS_HEALTHY`, `EXECUTION_STATUS_BLOCKED`, `EXECUTION_STATUS_FAILED`, or `EXECUTION_STATUS_ROLLED_BACK` |
| `Reason`, `Message` | Why the application is in that state, as a stable code and a sentence |
| `Desired` / `Observed` / `Active` | The generation you submitted, the one the controller has acted on, and the one serving traffic |
| Replica line, phase | `ALLOCATION_PHASE_PENDING`, `PULLING`, `STARTING`, `RUNNING`, `BACKOFF`, `STOPPED`, or `FAILED` |
| Replica line, `health=` | `HEALTH_STATUS_CHECKING`, `READY`, `UNHEALTHY`, or `NOT_CONFIGURED` |
| Replica line, `restarts=` and `reason=` | How often the process restarted and why the replica is in its state |
| Route line | Route state, `certificate=`, the number of ready `upstreams=`, and `reason=` |

`orch nodes` prints, for every node, its lifecycle and connection state (for
example `NODE_LIFECYCLE_ACTIVE/NODE_CONNECTION_STATUS_CONNECTED`), when it was
last seen, its capacity, labels, and a network line with `network=`,
`reason=`, `subnet=`, `mesh=` (advertise address and WireGuard port),
`dns=`, `ingress-role=`, `caddy=`, and `ingress=`.

## A node won't join

### What it looks like

`orch join` or `orch node install` exits with an error, or `orch nodes` does
not list the new node, or lists it but it never becomes connected.

### Inspect

Run the host checks on the worker. They change nothing:

```console
sudo orch node doctor --advertise-address 10.0.0.11
```

If an install already started, use the installed copy and look at the agent:

```console
sudo orch node doctor --advertise-address 10.0.0.11 --json
sudo systemctl status orch-containerd orch-agent
sudo journalctl -u orch-containerd -u orch-agent --since '-10 min'
```

From the workstation, list outstanding join tokens:

```console
orch node join list
```

### Fix

Match the error message:

| Error | Cause and fix |
| --- | --- |
| `run this command as root (for example with sudo)` | Rerun with `sudo`. |
| `could not detect this host's address; pass --advertise-address` | The host has no default route. Pass `--advertise-address IP` with an IPv4 address assigned to the host. |
| `advertise address must be a unicast IPv4 address on this host` | The address is not a usable IPv4 address. Pass one that is assigned to an interface on this host. |
| `host has installation blockers:` followed by a list | Each line names a failed check and its fix. See [`orch node doctor` findings](#orch-node-doctor-findings). |
| `join token expired at ...; create a new one with orch join-token` | Tokens last one hour by default. Create a new one, with `--ttl` up to `24h` if you need more time. |
| `join token must start with orch1`, `join token is malformed`, or `join token is incomplete` | The token was cut off while copying. Copy the whole printed command again, or pass it on stdin with `orch join -`. |
| `reach controller https://...: ...` | The worker cannot open a TLS connection to the controller. Check that the controller address in the token is reachable from the worker and that the controller port (9443 by default) is allowed from the worker. |
| `controller CA does not match the join token; check the controller address` | The address in the token answers with a different cluster's certificate, for example after reinstalling the controller or through a proxy. Create a token on the right controller. |
| `another node install is running` | Wait for the other install to finish. |
| `existing installation intent differs; upgrades and reconfiguration are unsupported` | The host was installed, or partly installed, with a different binary, advertise address, or join token. Rerun with the original binary, address, and token. To change the binary of a working node, use `orch node upgrade`. |
| `refusing unowned path: ...` or `Unowned installation path exists: ...` | Something orch would own already exists, usually from a manual install or a container runtime. Use a fresh host. |
| `installed, but agent readiness timed out; ...` | Everything is installed, but the agent has not reported a connected, network-ready state within five minutes. See below. |

An interrupted install resumes when you run the same command again with the
same binary, address, and token or join file. If enrollment already finished,
you can resume without the token:

```console
sudo orch node install --advertise-address 10.0.0.11
```

When the agent times out, read its journal. Common causes:

- The controller port is blocked from the worker, so the agent cannot keep its
  session.
- The workload network overlaps a host route; the journal and `orch nodes`
  show `network_cidr_conflict`. See [the workload network](#the-workload-network-conflicts-with-a-host-network).
- The WireGuard module is missing or the port is taken; the reason is
  `wireguard_unavailable`.

Fix the cause, then rerun the install command to resume waiting.

## `orch node doctor` findings

### What it looks like

`orch node doctor` prints a summary line such as
`Local node: ubuntu 24.04 amd64 (ready=false installable=true)`, then one line
per check in the form `[STATE] id: detail`, with the remedy on the next line.

- `ready` is true only when nothing fails.
- `installable` is true when every failing check is one that installation
  fixes itself.

On a fresh host, `ready=false installable=true` is normal: the pinned
dependencies are not installed yet and IPv4 forwarding may be off. Installation
fixes those. Only `installable=false` blocks `orch join` and
`orch node install`.

`warn` checks never block installation. They are reminders about things orch
cannot check from the host itself, such as provider firewalls.

### Fix

| Check | Failure | Fix |
| --- | --- | --- |
| `platform` | Not running on Linux | Run on Ubuntu 24.04 LTS or Fedora 44, amd64. |
| `distribution` | Unsupported OS, version, or architecture, or ostree-based Fedora | Use Ubuntu 24.04 LTS or Fedora 44 amd64. ostree-based Fedora editions are not supported. |
| `privilege` | `unknown`: not running as root | Run doctor with `sudo`; several checks need root. |
| `systemd` | systemd is not PID 1 | Use a VM or machine booted with systemd, not a plain container. |
| `cgroups` | cgroups v2 with `cpu`, `memory`, and `pids` is missing | Boot with the unified cgroup hierarchy (the default on both supported distributions) and reboot. |
| `advertise_address` | Address not assigned locally | Use an IPv4 address that appears in `ip -4 addr` on this host. On clouds with 1:1 NAT, the public address is not on the interface; advertise the private address. |
| `disk` | Less than 2 GiB free under `/var/lib` | Free space, or grow the file system that holds `/var/lib`. Images, logs, and volumes all live there. |
| `forwarding` | IPv4 forwarding is disabled | Installation writes `/etc/sysctl.d/90-orch.conf`; nothing to do. |
| `module_overlay`, `module_wireguard` | `fail`: available but not loaded | Installation loads and persists them; nothing to do. |
| `module_overlay`, `module_wireguard` | `unknown`: availability could not be established | Install a kernel that provides the module (and `modinfo`). On minimal cloud images, install the distribution's extra kernel modules package. |
| `containerd`, `runc`, `cni`, `caddy` | Pinned component is not installed | Installation installs it; nothing to do. |
| `containerd`, `runc`, `cni`, `caddy` | Ownership, checksum, or version could not be verified | An owned file was changed after installation. Inspect `/var/lib/orch-install/manifest.json` and the files under `/opt/orch`. |
| `caddy_capability` | Caddy lacks its exact low-port capability | Something removed or changed the file capability on `/opt/orch/bin/caddy`. Restore exactly `cap_net_bind_service=ep`, for example with `sudo setcap cap_net_bind_service=+ep /opt/orch/bin/caddy`. |
| `utility_nft`, `utility_ip`, `utility_setcap` | Utility is missing | Installation installs the packages; nothing to do. |
| `ownership` | Unowned path exists, or an unowned `orch-ingress` account exists | A previous manual install or another tool left files behind. Use a fresh host; orch does not adopt existing state. |
| `foreign_service` | `containerd.service`, `docker.service`, or `caddy.service` already exists | orch runs its own containerd and Caddy and does not replace existing services. Use a host without Docker, containerd, or Caddy. |
| `ports` | Conflicting listener | Something else listens on UDP or TCP 51820, on TCP 80 or 443 on all addresses or the advertise address, or on port 53 on all addresses. Stop or reconfigure it. A resolver on `127.0.0.53` is fine. |
| `ports`, `routes` | `unknown`: inspection unavailable | `ss` or `ip` is missing; installation installs `iproute2` or `iproute`. |
| `selinux` | `warn`: permissive or disabled | orch never changes the SELinux mode. If you enable SELinux later, rerun the install so it applies labels. |
| `selinux_context` | Unexpected SELinux context on an owned path | Restore labels on that path with `sudo restorecon -RF PATH`. |
| `firewalld` | `warn` with its state | Allow UDP 51820 between nodes and TCP 80 and 443 on ingress nodes in your zones. |
| `routes` | `warn` | Reminder that the workload network must not overlap host routes. The agent verifies it when it starts. |
| `peer_reachability` | `warn` | Reminder to allow UDP 51820 between nodes. Doctor cannot test provider firewalls. |
| `public_wireguard_endpoint` | `warn` | The advertise address is public. Allow UDP 51820 only from the other nodes' addresses. See [public advertise addresses](networking.md#public-advertise-addresses). |
| `agent_readiness` | No fresh authenticated, network-ready agent receipt | Only shown after installation. The agent is not connected or its network is not ready. Read `journalctl -u orch-agent`, check that the controller is reachable, then rerun the install to resume waiting. |

`orch cluster doctor` checks a controller host the same way. Its own checks
include `clock` (the system time must be plausible before certificates are
created), `controller_disk` (512 MiB free under `/var/lib`),
`controller_listener` (the port must be free or owned by orch),
`controller_certificate` (warns 30 days before the controller certificate
expires), and `controller_service` (the running service must be the owned
binary with the configured listener). Pass the same `--server` and `--listen`
values the controller was installed with; `orch init` uses
`https://ADDRESS:9443` and `0.0.0.0:9443` by default. Different values report
`Installed controller intent differs`.

## A deploy fails before it starts

### What it looks like

`orch validate`, `orch plan`, or `orch deploy` exits with status 2 because the
file is invalid, or `orch deploy` exits with status 1 and prints
`deploy request failed` because the controller rejected or did not receive
the request.

### Inspect and fix

| Error | Fix |
| --- | --- |
| `Pkl 0.32.1 is required; install the repository toolchain with mise install` | Install Pkl 0.32.1 on the workstation and put it on `PATH`. |
| `manifest is invalid` followed by `path: message` lines | Each line names the field and the rule it breaks, for example `services.database.replicas: must be 1 when the service uses local volumes`. Fix the file and run `orch validate` again. See the [application manifest](manifest.md). |
| `services.NAME.image: image or tag was not found; check the repository and tag` | The controller could not find the tag. Check the spelling and that the tag was pushed. |
| `registry authentication failed; update credentials with orch registry login` | The image is private, or the stored credentials are wrong. Run `orch registry login HOST --username USER --password-stdin`. |
| `registry could not resolve the image; check registry connectivity and TLS` | The controller resolves tags to digests, so the controller host needs outbound HTTPS to the registry. Check its DNS, proxy, and firewall. |
| `image has no supported Linux amd64 or arm64 platform` | Build or pull an image for `linux/amd64`. |
| `deploy request failed; retry with --idempotency-key KEY` | The request may or may not have reached the controller. Retry with the printed key; a retry with the same key cannot apply the change twice. |

## A deploy is stuck or blocked

### What it looks like

`orch deploy --wait` keeps printing progress and ends with `TIMED_OUT`, or
`orch app status` shows `EXECUTION_STATUS_BLOCKED` or
`EXECUTION_STATUS_PENDING` for a long time.

Stopping `--wait` does not cancel the deployment. Watch it again with:

```console
orch app watch APP
```

### Inspect

```console
orch app status APP
orch nodes
```

Read `Reason` and `Message` on the application, and `reason=` on each replica.

### Fix

| Reason | Meaning and fix |
| --- | --- |
| `secret_missing` | A referenced secret is not set for this application. Set it with `orch secret set APP NAME --from-stdin`. The previous revision keeps running meanwhile. |
| `secret_environment_invalid` | A secret used in `secretEnv` is not valid UTF-8 or contains a NUL byte, so it cannot be an environment variable. Project it as a file with `secretFiles` instead. |
| `constraint_mismatch` | No node has every label in `constraints`. Add the label with `orch node label NODE KEY=VALUE`, or correct the constraint. |
| `insufficient_cpu`, `insufficient_memory` | No eligible node has enough unreserved CPU or memory. Compare `allocatable` and `assigned` in `orch nodes`, then lower `resources`, reduce replicas, or add a node. |
| `no_nodes` | No node is enrolled and ready. Join a worker. |
| `node_cordoned`, `node_draining` | The only eligible nodes are cordoned or draining. Run `orch node uncordon NODE` when maintenance is over. |
| `node_unreachable`, `node_not_ready` | The eligible node is not connected or not ready. See [a worker is disconnected](#a-worker-is-disconnected). |
| `volume_node_lost` | The worker that holds a service's local volume is gone. orch will not create an empty volume elsewhere. Bring that worker back, or restore the data separately. |
| `local_volume_pinned` | The service's volume lives on a node that cannot run it right now, for example because it is cordoned, draining, or out of capacity. The message names the node. Make room on that node or uncordon it. |
| `local_volume_conflict` | One service mounts several volumes that already live on different nodes. Every volume a service mounts must live on the node that runs it. |
| `node_port_in_use` | Every eligible node already runs a replica that publishes the same node port. Add a node or reduce replicas. |
| `node_port_conflict`, `tcp_port_conflict` | Another application already owns that published port. |
| `domain_conflict`, `http_public_conflict` | Another application already owns the domain or the catch-all HTTP listener. Remove it from that application first. |
| `network_initializing` | The node has not finished preparing the workload network. Check `network=` and `reason=` in `orch nodes`. |
| `pre_deploy_waiting` | The pre-deploy hook cannot be placed: no node has room or a secret it needs is missing. |
| `pre_deploy_running` | The pre-deploy hook is still running. Follow it with `orch job logs JOB_ID`; the job ID is in the message. |
| `pre_deploy_failed` | The hook failed, so the new revision was not rolled out. Read `orch job logs JOB_ID`, fix the cause, and run `orch job retry JOB_ID`, or deploy a corrected file. |
| `dependency_wait` | A replica or cron run waits for a service in `dependsOn` to become available. Fix that service first. |
| `rollout_budget_wait` | On a replica: it is queued until the rollout's `maxSurge` or `maxUnavailable` budget allows it to start. This is normal during a rollout and clears as earlier replicas become ready. If it does not clear, look at the replicas ahead of it. |
| `containerd_unavailable`, `unsupported_runtime_version`, `cni_plugins_unavailable`, `nftables_required`, `dns_unavailable`, `caddy_unavailable`, `cgroups_v2_required` | The node's runtime does not match what this orch release expects. Run `orch node doctor` on that node and check its journal. |

## A deploy was rolled back or failed

### What it looks like

`orch deploy --wait` ends with `FAILED`, `orch app status` shows
`EXECUTION_STATUS_ROLLED_BACK` or `EXECUTION_STATUS_FAILED`, or replicas report
`health_gate_rollback` or `replacement_rollback_retained`.

With `autoRollback = true`, a revision whose replicas do not become ready is
abandoned and the last healthy revision keeps or resumes serving traffic.

### Inspect

```console
orch app history APP
orch app status APP
orch logs APP SERVICE --replica N --tail 200
```

`orch app history` shows the outcome and reason for each generation. The
replica logs usually show why the new version did not become ready.

### Fix

Correct the image or configuration and deploy again. To return to an earlier
revision on purpose, see [rollbacks](runtime.md#rollbacks).

| Replica reason | Meaning |
| --- | --- |
| `image_pull_failed` | The node could not pull the image. `orch app status` prints the registry error under the replica. orch retries with a growing delay. See [disk is full or images fail to pull](#disk-is-full-or-images-fail-to-pull). |
| `process_start_backoff` | The container could not be created or started within `deploy.healthDeadline`, for example because the pull took too long or the command does not exist. orch retries with a growing delay and counts each attempt in `restarts=`. See [disk is full or images fail to pull](#disk-is-full-or-images-fail-to-pull). |
| `process_start_failed` | The container could not start and `restart = "never"`. |
| `process_exit_backoff` | The process exited and is waiting to restart. Read its logs. |
| `restart_policy_complete` | The process exited and its restart policy does not restart it. |
| `health_deadline_exceeded` | The replica did not become ready within `deploy.healthDeadline` (two minutes by default). |
| `health_gate_rollback`, `rollback_unhealthy`, `replacement_rollback_retained` | The new revision failed and orch kept or restored the previous one. |

## Health checks fail

### What it looks like

Replicas stay at `health=HEALTH_STATUS_CHECKING` or turn
`HEALTH_STATUS_UNHEALTHY` with `reason=readiness_unhealthy`, routes report no
ready upstreams, and rollouts end in `health_deadline_exceeded`.

### How the check works

- An HTTP service with `healthPath` gets an HTTP GET to that path every two
  seconds. A response from 200 to 399 counts as success. Redirects are not
  followed.
- Each probe has a one-second timeout.
- Two consecutive successes make a replica ready. Three consecutive failures
  make it unhealthy.
- An HTTP service without `healthPath`, and every TCP service, gets a TCP
  connection check on its port instead.
- The probe connects to the replica's private address, not to `localhost`,
  and sends `Host: SERVICE.APPLICATION.svc.internal`.

### Inspect

```console
orch app status APP
orch logs APP SERVICE --replica N --tail 100
orch exec APP SERVICE --replica N -- wget -qO- http://127.0.0.1:8080/healthz
```

Use whatever HTTP client the image provides for the last command.

### Fix

- Make the application listen on all interfaces (`0.0.0.0`) on the declared
  port, not only on `127.0.0.1`. A check from inside the container can
  succeed while the probe from the node fails.
- Make sure `http.port` or `tcp.port` matches the port the process listens on.
- Make `/healthz` answer within a second, without depending on databases or
  other services that may be briefly unavailable.
- Accept any `Host` header on the health path; the probe uses the cluster
  service name.
- If the application needs longer to start, for example on a cold node that
  has to pull a large image, raise `deploy.healthDeadline` (up to `1h`).

## The HTTPS certificate is not issued

### What it looks like

Browsers show a certificate error or the connection fails on port 443, while
`orch app status` shows the route.

`certificate=managed` on a route line means Caddy is responsible for the
certificate; it does not prove a certificate was issued. A route that has not
been published yet shows `reason=readiness_pending` until the service is
healthy.

### Inspect

From your workstation:

```console
dig +short app.example.com
curl -vI https://app.example.com
orch nodes
```

`dig` must return the address of an ingress-enabled node (`ingress-role=true`
in `orch nodes`). On that node, read Caddy's log, which names the failing
certificate request:

```console
sudo tail -n 100 /var/lib/orch-ingress/caddy.log
```

### Fix

- Point the domain's A record at every ingress-enabled node, and only at
  those. A record that points at a node without ingress, at the controller, or
  at an old address makes validation fail.
- Allow TCP 80 and 443 from the internet to the ingress nodes in host and
  provider firewalls. The certificate authority connects to them to validate
  the domain.
- Wait for DNS changes to propagate, then watch the log again. Caddy retries
  on its own.
- Make sure the service is healthy. Routes, and therefore certificates, are
  only published for healthy services.
- If a route reports `caddy_unavailable` or `ingress_apply_failed`, Caddy on
  that node could not start or accept the configuration. Read
  `journalctl -u orch-agent` on the node.

## Service discovery or DNS fails

### What it looks like

A service cannot resolve `SERVICE.APPLICATION.svc.internal`, or resolves it
but cannot connect.

### Inspect

```console
orch app status APP
orch nodes
orch exec APP SERVICE --replica 1 -- cat /etc/resolv.conf
orch exec APP SERVICE --replica 1 -- nslookup database.notes.svc.internal
```

In `orch nodes`, check `dns=` and `reason=` on the node that runs the client.

### Fix

- Check the name. It is `SERVICE.APPLICATION.svc.internal`, built from the
  names in `orch.pkl`, for example `database.notes.svc.internal`.
- Only running, ready replicas are returned. If the target replicas are
  `HEALTH_STATUS_CHECKING` or `HEALTH_STATUS_UNHEALTHY`, fix them first; see
  [health checks fail](#health-checks-fail).
- Use the port from the target's `http.port` or `tcp.port`.
- If `dns=` is not ready or the reason is `dns_start_failed` or
  `dns_unavailable`, the node's DNS server could not start. It listens on
  port 53 on the node's workload gateway; read the agent journal on that node.
- If names resolve but connections time out only when the client and target
  are on different nodes, the mesh between them is down; see the next section.

## Workers can't reach each other over WireGuard

### What it looks like

Services on the same node work, but connections between replicas on different
nodes, or from an ingress node to replicas on other nodes, time out. Routes may
show fewer `upstreams=` than ready replicas.

### Inspect

In `orch nodes`, every node should show `network=` ready, a `mesh=` address
that the other nodes can reach, and `key=true`. A `reason=` of
`wireguard_unavailable` means the WireGuard interface could not be set up on
that node.

On a worker:

```console
ip -br link show type wireguard
ip -4 route
sudo ss -lunp | grep 51820
```

The interface is named `orchwg` followed by nine hexadecimal characters. If
you install your distribution's `wireguard-tools` package, `sudo wg show`
also prints the latest handshake with each peer; a peer with no recent
handshake cannot be reached.

### Fix

- Allow UDP 51820 between every pair of workers, in both directions, in the
  host firewall and in provider firewalls or security groups. With public
  advertise addresses, allow it only from the other workers' addresses.
- Make sure every node advertises an address the others can reach, and that
  no two nodes advertise the same address. The advertise address is fixed at
  install time.
- Allow forwarding between the host interface, the WireGuard interface, and
  the workload network. With UFW enabled, the default routed policy is `deny`;
  on Fedora, configure firewalld zones and policies.
- If `wireguard_unavailable` persists, check that the `wireguard` module is
  loaded (`lsmod | grep wireguard`) and that nothing else listens on UDP
  51820.

See [networking](networking.md#troubleshooting) for the full checklist.

## The workload network conflicts with a host network

### What it looks like

A node reports `network_cidr_conflict` in `orch nodes`, and the agent journal
says the workload CIDR overlaps a host address or route.

### Fix

Before the first node finishes preparing its network, choose another range:

```console
orch network configure --cidr 10.99.0.0/16
```

The workload network is fixed once a node has prepared it. After that,
`orch network configure` with a different range fails with
`network is already initialized`, and `orch status` shows `frozen true`. In
that case, change the conflicting route or VPN on the host instead.

## The controller is unreachable

### What it looks like

CLI commands fail with a connection or TLS error, `orch target test` fails, or
every node shows as disconnected.

Running workloads are not affected. Workers keep running their last assigned
workloads while the controller is down. Deployments, scheduling, node changes,
and secret changes wait until it returns.

### Inspect

From the workstation:

```console
orch target list
orch target test
```

For an SSH target, check that plain SSH works without a prompt. orch runs
`ssh` in batch mode, so it cannot ask for a password or confirm an unknown
host key:

```console
ssh root@10.0.0.10 true
```

On the controller:

```console
sudo systemctl status orch-controller
sudo journalctl -u orch-controller --since '-10 min'
sudo orch status
sudo orch cluster doctor --server https://10.0.0.10:9443 --listen 0.0.0.0:9443
```

On the controller host, root can run any `orch` command against the local
administrator socket, so `orch status` there works even when the network path
from your workstation does not. The installed binary is
`/opt/orch-controller/bin/orch`, linked as `/usr/local/bin/orch`.

### Fix

| Symptom | Fix |
| --- | --- |
| `no target is selected; ...` | Add or select a target: `orch target add NAME ssh://root@CONTROLLER` or `orch target use NAME`. |
| `no controller answered through HOST (run orch init there and log in as root)` | SSH works, but the administrator socket is not there: the controller is not installed, not running, or you logged in as a user other than root. |
| SSH asks for a password or host key | Load your key into `ssh-agent` or configure it in `~/.ssh/config`, and connect once by hand to accept the host key. |
| `orch-controller` is not active | Read its journal. Start it with `sudo systemctl start orch-controller`. |
| `controller_certificate` fails or warns in `orch cluster doctor` | The controller certificate expired or is about to. Renew it; see [certificate renewal](security.md#certificate-renewal). Certificate targets also need a renewed administrator context. |
| Workers disconnected, controller running | Allow the controller port (9443 by default) from the workers, and check that the controller is still reachable at the address workers were enrolled with. |
| The controller host is lost | Restore it from a backup at the same HTTPS address; see [recovery](security.md#recovery). |

## A worker is disconnected

### What it looks like

`orch nodes` shows `NODE_CONNECTION_STATUS_DISCONNECTED` or
`NODE_CONNECTION_STATUS_UNREACHABLE`, and `last seen` is not recent.

After about a minute without contact, the controller treats the node as lost.
Services without local volumes are placed on other eligible nodes. Services
with a local volume on that node report `volume_node_lost` and wait for it.

### Inspect

On the worker:

```console
sudo systemctl status orch-agent orch-containerd
sudo journalctl -u orch-agent --since '-10 min'
sudo orch node doctor --advertise-address 10.0.0.11
```

### Fix

- Start the services if they stopped:
  `sudo systemctl start orch-containerd orch-agent`.
- Make sure the worker can reach the controller port.
- Check `certificate=` and `expires=` in `orch nodes`. Worker certificates
  renew automatically.
- If the host is gone for good, remove it with `orch node remove NODE` and
  join a replacement. Data in its local volumes is lost with the host unless
  you restore it from a backup.

## Disk is full or images fail to pull

### What it looks like

Replicas alternate between `ALLOCATION_PHASE_STARTING` and
`ALLOCATION_PHASE_BACKOFF` with a growing `restarts=` count, or the host runs
out of space. When the registry refuses or cannot serve the image, the reason
is `image_pull_failed` and `orch app status` prints the registry's error under
the replica, for example:

```text
  game/1  ALLOCATION_PHASE_BACKOFF  ... reason=image_pull_failed
    pulling ghcr.io/acme/game@sha256:... failed: ... 401 Unauthorized; waiting before retry
```

A `401` or `403` usually means a missing or wrong registry credential; see
[secrets and registry credentials](secrets.md). With
`reason=process_start_backoff` the image was available but the container did
not start, so check the likely causes below one by one.

### Inspect

On the worker:

```console
df -h /var/lib
sudo du -sh /var/lib/orch-containerd /var/lib/orch-agent/logs /var/lib/orch-agent/volumes
curl -sSI https://ghcr.io/v2/
```

The last command checks that the worker reaches the registry over HTTPS; use
your registry's host name. Any HTTP response, including `401`, means the
connection works.

Images live in `/var/lib/orch-containerd`, workload logs in
`/var/lib/orch-agent/logs`, and local volumes in
`/var/lib/orch-agent/volumes`. See the [host reference](host.md) for every
path.

### Fix

- **Space.** Grow the file system that holds `/var/lib`, or move data off it.
  Volumes are never deleted by orch, including after `orch remove`; delete the
  directories of applications you have removed and no longer need. Do not
  delete files under `/var/lib/orch-containerd` by hand; orch's containerd
  owns them.
- **Registry access.** Workers pull images directly from the registry, so each
  worker needs outbound HTTPS to it. The controller resolves tags first, so a
  deploy can be accepted while a worker that cannot reach the registry fails
  to pull.
- **Private images.** Store the credentials once with `orch registry login`;
  the controller delivers them to the workers that pull the image.
- **Slow first pulls.** Creating and starting a replica, including the image
  pull, must finish within `deploy.healthDeadline` (two minutes by default).
  A large image on a new or slow node can take longer. Raise the deadline, for
  example to `"5m"`.
- **Wrong command or platform.** If the image is fine, check `command`,
  `args`, and that the image was built for `linux/amd64`. Running the same
  image and command with `orch job run` shows the job's state and exit
  code.
- **Upgrades.** `orch node upgrade` needs 256 MiB free and
  `orch cluster upgrade` needs 512 MiB free for their recovery copies.

## Still stuck

Collect the following and compare them with the sections above:

```console
orch status --json
orch nodes --json
orch app status APP --json
orch events --limit 200 --json
sudo journalctl -u orch-agent -u orch-containerd --since '-1 hour'   # each affected worker
sudo journalctl -u orch-controller --since '-1 hour'                 # controller
```

Check the [limitations](product.md) to make sure the setup is supported, and
the [host reference](host.md) for what orch expects from each host.
