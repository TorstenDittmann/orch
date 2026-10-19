# Host reference

This page lists what orch expects from a host and everything it changes on
one: supported operating systems, network ports, the files, accounts, and
systemd units it owns, the pinned dependencies it installs, and the capacity
it keeps back for the host itself. Use it to prepare firewalls and images, to
review a host before installation, or to find a file while debugging.

For the step-by-step setup, see [installation](installation.md). For
symptoms and fixes, see [troubleshooting](troubleshooting.md).

## Supported hosts

| Requirement | Supported |
| --- | --- |
| Operating system | Ubuntu 24.04 LTS, or Fedora 44 (any edition except ostree-based ones such as Silverblue or CoreOS, whose `/usr` is read-only) |
| Architecture | amd64 |
| Init system | systemd running as PID 1 |
| Control groups | Unified cgroups v2 with the `cpu`, `memory`, and `pids` controllers |
| Kernel modules | `overlay` and `wireguard` (installation loads them) |
| Address | A unicast IPv4 address assigned to a local interface, private or public |
| Free disk under `/var/lib` | 512 MiB for a controller, 2 GiB for a worker |
| Clock | Synchronized; the controller refuses a year before 2024 or after 2100 when it creates certificates |
| Privileges | Root for `orch init`, `orch join`, `orch cluster ...`, and `orch node ...` host commands |

The same binary and the same checks are used for both roles. A controller and
a worker can share one host: their files, accounts, and services do not
overlap.

The advertise address must be assigned to an interface on the host itself.
`orch node doctor` compares it with the host's interface addresses. On clouds
that map a public address to the host through 1:1 NAT, so that the public
address never appears on an interface, advertise the private address instead.

Start from a fresh host. Installation refuses to adopt existing state: a
worker install fails when `containerd.service`, `docker.service`, or
`caddy.service` is already loaded, when an `orch-ingress` account already
exists, or when any path orch would own already exists. A controller install
fails on an existing `orch-controller` account or controller path.

Check a host before installing anything:

```console
sudo orch node doctor --advertise-address 10.0.0.11
sudo orch cluster doctor --server https://10.0.0.10:9443 --listen 0.0.0.0:9443
```

Both commands only inspect. Each check prints a state (`pass`, `warn`, `fail`
or `unknown`) and, when there is one, the remedy. See
[`orch node doctor` findings](troubleshooting.md) for each check.

## Network ports

orch never changes UFW, firewalld, nftables rules outside its own tables, or
cloud firewalls. Open these ports yourself.

### Inbound

| Port | Protocol | On | From | Purpose |
| --- | --- | --- | --- | --- |
| 9443 (`orch init --port`, or the `--listen` port of `orch cluster install`) | TCP | Controller | Workers, operators that use certificate targets, and GitHub Actions runners when you use `orch github trust` | Controller API: agent sessions, enrollment, administration over mTLS |
| 22 | TCP | Controller | Operators | SSH targets (`orch target add NAME ssh://root@HOST`) reach the administrator socket over SSH |
| 51820 | UDP | Every worker | Every other worker | WireGuard mesh between nodes. With public advertise addresses, allow it only from the other workers' addresses |
| 80 | TCP | Ingress-enabled workers | Clients, and the ACME certificate authority | HTTP routes, the `public = true` catch-all listener, and HTTP certificate challenges |
| 443 | TCP | Ingress-enabled workers | Clients, and the ACME certificate authority | HTTPS routes |
| Each `tcp.publicPort` | TCP | Ingress-enabled workers | Clients | TCP ingress, forwarded to ready replicas |
| Each `tcp.nodePort` / `udp.nodePort` | TCP or UDP | Workers that run the service | Clients | Node-local ports published straight to the replica on that node |

The controller listen port must be 1024 or higher. `orch init` listens on
`0.0.0.0` at that port and advertises `https://ADDRESS:PORT` to workers.

Some ports on workers are reserved: TCP 53, 80, and 443 and UDP 53 and 51820
cannot be used as `publicPort` or `nodePort` values.

### Local listeners

These are used by orch but should not be opened in a firewall.

| Listener | Where | Purpose |
| --- | --- | --- |
| Port 53, TCP and UDP, on the node's workload gateway address | Workers | Cluster DNS for `SERVICE.APPLICATION.svc.internal` names, served to containers on that node |
| `/var/lib/orch-controller/admin.sock` | Controller | Local administrator socket; only root and the controller account can open it |
| `/var/lib/orch-ingress/admin.sock` | Workers | Caddy's admin API, used only by the agent |
| `/run/orch-containerd/containerd.sock` | Workers | orch's dedicated containerd |
| `127.0.0.1`, random or `--port` | Workstation | `orch web` dashboard |

Because the cluster DNS server runs on every worker, `orch node doctor` fails
when another process listens on port 53 on all addresses (`0.0.0.0`, `*`, or
`::`). A resolver bound only to a loopback address, such as systemd-resolved
on `127.0.0.53`, does not conflict. It also fails when anything other than
orch listens on UDP 51820, or on TCP 80 or 443 on all addresses or the
advertise address.

### Outbound

| From | To | Purpose |
| --- | --- | --- |
| Workers | Distribution package mirrors | `apt-get` or `dnf` installs the OS utilities listed below |
| Workers | `github.com` over HTTPS | Download of the pinned containerd, runc, CNI plugins, and Caddy releases during `orch join` or `orch node install` |
| Workers | The controller port | Enrollment and the agent's session |
| Controller | Container registries over HTTPS | `orch deploy` resolves image tags to digests on the controller |
| Controller | `token.actions.githubusercontent.com` over HTTPS | Only with `orch github trust`: GitHub's signing keys for workflow tokens |
| Workers | Container registries over HTTPS | Image pulls |
| Ingress-enabled workers | The ACME certificate authority | Automatic HTTPS certificates through Caddy |

### Forwarding

Workers forward packets between the host interface, the WireGuard interface,
and the workload network (`10.88.0.0/16` unless you chose another range with
`orch network configure`). Installation enables IPv4 forwarding, but a host or
provider firewall can still drop forwarded traffic. Allow it. On Ubuntu with
UFW enabled, the default routed policy is `deny`; on Fedora, configure
firewalld zones and policies. See
[networking requirements](networking.md#requirements).

## Controller host

`orch init` and `orch cluster install` install the controller. `orch init`
fills in the options: it detects the advertise address, listens on
`0.0.0.0:9443`, names the cluster `orch`, and writes the break-glass
administrator context to `/etc/orch/admin`. `orch cluster install` takes each
of these explicitly.

### Files and directories

| Path | Contents |
| --- | --- |
| `/opt/orch-controller/bin/orch` | Copy of the orch binary that ran the install; root-owned |
| `/usr/local/bin/orch` | Link to the controller's binary, so `sudo orch` works; replaces a node's link |
| `/var/lib/orch-controller/` | Controller state, owned by `orch-controller` |
| `/var/lib/orch-controller/control.db` | SQLite database: desired state, revisions, encrypted secrets, history |
| `/var/lib/orch-controller/secret-master.key` | Secret-encryption key |
| `/var/lib/orch-controller/pki/` | Cluster CA and controller certificate and keys (`ca.pem`, `ca-key.pem`, `controller.pem`, `controller-key.pem`) |
| `/var/lib/orch-controller/admin.sock` | Local administrator socket |
| `/var/lib/orch-controller-install/manifest.json` | Installation journal: options, owned files and their hashes, completed phases |
| `/var/lib/orch-controller-install/upgrades/` | Recovery sets created by `orch cluster upgrade` |
| `/etc/systemd/system/orch-controller.service` | systemd unit |
| `/etc/orch/admin/` | Break-glass mTLS administrator context from `orch init` (`--admin-dir` with `orch cluster install`) |
| `--recovery-dir` | Optional day-zero recovery set: database backup, secret key, and PKI |
| `/run/lock/orch-controller-install.lock` | Lock held while an install runs |

### Account

`orch-controller` is a system account and group with home directory
`/var/lib/orch-controller` and a `nologin` shell. The service runs as this
account. The binary and unit stay root-owned.

### systemd unit

`orch-controller.service` runs:

```text
/opt/orch-controller/bin/orch controller serve --data-dir /var/lib/orch-controller --listen 0.0.0.0:9443
```

It restarts on failure and is hardened with `NoNewPrivileges`,
`ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`, and
write access only to `/var/lib/orch-controller`.

### What a controller install changes

1. Creates the `orch-controller` account.
2. Copies the running orch binary to `/opt/orch-controller/bin/orch` and links
   it as `/usr/local/bin/orch`, replacing a copy of the same binary but no other
   file that orch did not create.
3. Creates the cluster identity: CA, controller certificate, database, and
   secret key, plus the administrator context.
4. Writes and verifies the systemd unit (`systemd-analyze verify`). On Fedora
   with SELinux enabled, restores file labels on the owned paths.
5. Enables and starts `orch-controller.service` and waits until it answers
   with the new cluster identity.
6. Writes a verified recovery set when you pass `--recovery-dir`.

It leaves firewall rules, DNS, routing, sysctl settings, and unrelated services
unchanged. It installs no packages.

Each phase is recorded in the installation journal. Rerunning the same command
with the same binary and options resumes from the first unfinished phase. A
different binary, address, or option is refused with "existing controller
installation intent differs"; use `orch cluster upgrade` to change the binary.

## Worker host

`orch join` and `orch node install` install a worker. `orch join` resolves the
join token, contacts the controller to verify its CA, then performs the same
installation as `orch node install --join-file`.

Preview the plan, including every package, artifact, and file, without
changing the host or reading join credentials:

```console
sudo orch node install --advertise-address 10.0.0.11 --dry-run
```

### Installed dependencies

orch downloads these from the upstream GitHub releases, checks each SHA-256
against the digest embedded in the orch binary, and installs only the listed
executables. No upstream installer script runs.

| Component | Version | Installed files |
| --- | --- | --- |
| containerd (static build) | 2.3.4 | `/opt/orch/bin/containerd`, `/opt/orch/bin/containerd-shim-runc-v2` |
| runc | 1.4.3 | `/opt/orch/bin/runc` |
| CNI plugins | 1.9.1 | `/opt/orch/cni/bridge`, `/opt/orch/cni/host-local`, `/opt/orch/cni/loopback` |
| Caddy | 2.11.3 | `/opt/orch/bin/caddy` |

The versions are fixed for a given orch release. The controller only places
workloads on a node that reports containerd 2.3.4 and CNI plugins v1.9.1, and
only routes ingress through a node that reports Caddy v2.11.3. To change them,
upgrade orch.

It also installs these packages from the distribution's signed repositories,
without upgrading the rest of the host:

| Distribution | Packages |
| --- | --- |
| Ubuntu 24.04 | `ca-certificates`, `nftables`, `iproute2`, `libcap2-bin`, `kmod`, `passwd`, `udev` |
| Fedora 44 | `ca-certificates`, `nftables`, `iproute`, `libcap`, `kmod`, `shadow-utils`, `container-selinux`, `policycoreutils-python-utils`, `systemd-udev` |

### Files and directories

| Path | Contents |
| --- | --- |
| `/opt/orch/bin/orch` | Copy of the orch binary that ran the install |
| `/usr/local/bin/orch` | Link to `/opt/orch/bin/orch` unless the controller's link is already there |
| `/opt/orch/bin/`, `/opt/orch/cni/` | Pinned dependencies from the table above |
| `/opt/orch/containerd.toml` | Configuration for orch's dedicated containerd |
| `/etc/systemd/system/orch-containerd.service` | systemd unit for containerd |
| `/etc/systemd/system/orch-agent.service` | systemd unit for the agent |
| `/etc/sysctl.d/90-orch.conf` | `net.ipv4.ip_forward=1` |
| `/etc/modules-load.d/orch.conf` | Loads `overlay` and `wireguard` at boot |
| `/etc/systemd/network/10-orch-veth.link` | Keeps the MAC address of new `veth*` devices so CNI can recognize them later; it does not match physical NICs or change existing interfaces |
| `/var/lib/orch-install/manifest.json` | Installation journal: address, distribution, owned files and their hashes, completed phases, node ID |
| `/var/lib/orch-install/upgrades/` | Agent state backups created by `orch node upgrade` |
| `/var/lib/orch-agent/` | Agent state (mode 0700) |
| `/var/lib/orch-agent/agent.json`, `pki/` | Node identity: configuration, cluster CA, agent certificate and key |
| `/var/lib/orch-agent/agent.db` | Local cache of the assignment, so workloads survive a controller outage |
| `/var/lib/orch-agent/logs/` | Workload logs |
| `/var/lib/orch-agent/volumes/APPLICATION/VOLUME/` | Local volume data; kept when the application is removed |
| `/var/lib/orch-agent/volume-backups/APPLICATION/VOLUME/` | Volume backups taken on this node |
| `/var/lib/orch-agent/runtime-secrets/` | Secret files projected into containers |
| `/var/lib/orch-agent/network/` | CNI and WireGuard state and the containers' `resolv.conf` |
| `/var/lib/orch-containerd/` | containerd content store and snapshots (images) |
| `/run/orch-containerd/` | containerd runtime state and socket |
| `/var/lib/orch-ingress/` | Caddy data (certificates), admin socket, `caddy.pid`, and `caddy.log`; owned by `orch-ingress` |
| `/run/lock/orch-install.lock` | Lock held while an install runs |

Local volume data, volume backups, and workload logs live under
`/var/lib/orch-agent`. Size the file system that holds `/var/lib` for images,
logs, volumes, and their backups, not only for the 2 GiB the installer checks.
Each volume backup is a compressed copy of the whole volume, and `keep` of
them are retained.

### Account

`orch-ingress` is a system account and group with home directory
`/var/lib/orch-ingress` and a `nologin` shell. The agent runs Caddy as this
account. Installation gives `/opt/orch/bin/caddy` exactly one file capability,
`cap_net_bind_service=ep`, so Caddy can bind ports 80 and 443 without running
as root. There is no separate Caddy systemd service.

### systemd units

| Unit | Runs | Notes |
| --- | --- | --- |
| `orch-containerd.service` | `/opt/orch/bin/containerd --config /opt/orch/containerd.toml` | Dedicated containerd with its own socket and state paths; the CRI plugins are disabled. Restarts always |
| `orch-agent.service` | `/opt/orch/bin/orch agent serve ...` | Requires `orch-containerd.service`. Restarts on failure. `KillMode=process`, so restarting the agent does not stop running containers |

The agent unit passes these settings, all fixed at install time:

| Flag | Value |
| --- | --- |
| `--advertise-address` | The address given to `orch join` or `orch node install` |
| `--wireguard-port` | `51820` |
| `--system-reserved-cpu` | `500m` |
| `--system-reserved-memory` | `512Mi` |
| `--containerd-socket` | `/run/orch-containerd/containerd.sock` |
| `--snapshotter` | `overlayfs` |
| `--cni-bin-dir` | `/opt/orch/cni` |
| `--caddy-binary` | `/opt/orch/bin/caddy` |
| `--ingress-user` | `orch-ingress` |
| `--ingress-data-dir` | `/var/lib/orch-ingress` |

### Resource reservations

Each worker keeps 500 millicpu and 512 MiB of memory back for the host and
orch's own processes. The rest is allocatable to workloads. `orch nodes` shows
each node's total capacity, allocatable capacity, and what is already
assigned:

```console
orch nodes
```

The scheduler places a replica only on a node whose remaining allocatable CPU
and memory cover the service's `resources.cpu` and `resources.memory`. When no
node has room, the service is blocked with `insufficient_cpu` or
`insufficient_memory`.

### Network objects

The agent creates and owns these, named after the cluster and node:

| Object | Name |
| --- | --- |
| WireGuard interface | `orchwg` followed by 9 hexadecimal characters |
| Workload bridge | `orch` followed by 11 hexadecimal characters |
| nftables table | `orch_` followed by a digest, for forwarding, NAT, and node-local ports |

Each node receives one `/24` from the workload network. Containers on that
node get addresses from it, and the first usable address is the node's
gateway and DNS server.

### What a worker install changes

1. Installs the OS utilities listed above with `apt-get` or `dnf`.
2. Downloads and verifies the pinned artifacts and copies the orch binary to
   `/opt/orch/bin/orch`, linked as `/usr/local/bin/orch` on PATH.
3. Creates `/var/lib/orch-agent`, `/var/lib/orch-containerd`, and
   `/var/lib/orch-ingress`; creates the `orch-ingress` account; sets Caddy's
   low-port capability; writes the containerd configuration and both units;
   enables IPv4 forwarding; loads `overlay` and `wireguard` and makes them
   persistent; installs the veth link policy and reloads udev rules (without
   reapplying them to existing interfaces). On Fedora with SELinux enabled, it
   adds file-context mappings for the runtime paths using the distribution's
   `container-selinux` types and restores labels. It never changes the SELinux
   mode.
4. Enrolls with the controller using the join token or file.
5. Enables and starts `orch-containerd.service` and `orch-agent.service`.
6. Waits until the agent reports an authenticated assignment and a ready
   network (five minutes by default; `--wait-timeout` changes it).

It leaves host and provider firewall rules unchanged.

If installation stops, rerun the same command with the same binary, address,
and join token or file. It resumes from the first unfinished phase. A
readiness timeout leaves everything installed and running. An install with a
different address or binary is refused with "existing installation intent
differs; upgrades and reconfiguration are unsupported".

The first node that joins a cluster becomes ingress-enabled automatically.
Later nodes accept public traffic only after
`orch node ingress NODE --enable`.

## Upgrades

`orch cluster upgrade` and `orch node upgrade` replace the managed orch binary
in place. Before the switch, a controller upgrade writes a recovery set below
`/var/lib/orch-controller-install/upgrades/` and needs 512 MiB free; a worker
upgrade copies the agent state below `/var/lib/orch-install/upgrades/` and
needs 256 MiB free. A worker upgrade restarts the agent while containers keep
running under containerd. See [upgrades](installation.md#upgrades).

## Removing orch

orch has no uninstall command. To retire a worker, drain it and remove it from
the cluster with `orch node drain` and `orch node remove`, then reinstall or
discard the host. Local volume data under `/var/lib/orch-agent/volumes` is not
moved for you; copy it off first if you need it.
