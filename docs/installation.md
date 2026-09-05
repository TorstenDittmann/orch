# Guided installation

## Guided controller installation

On a fresh Ubuntu 24.04 LTS amd64 or Fedora Server 44 amd64 host, copy a trusted
Linux amd64 orch executable owned by root. Choose the canonical HTTPS endpoint
workers and operators will use and an unprivileged local listen port:

~~~console
sudo ./orch cluster doctor \
  --server https://10.0.0.10:9443 --listen 0.0.0.0:9443
sudo ./orch cluster install \
  --name production \
  --server https://10.0.0.10:9443 \
  --listen 0.0.0.0:9443 \
  --admin-dir /root/orch-production-admin \
  --recovery-dir /root/orch-production-recovery \
  --dry-run --json
sudo ./orch cluster install \
  --name production \
  --server https://10.0.0.10:9443 \
  --listen 0.0.0.0:9443 \
  --admin-dir /root/orch-production-admin \
  --recovery-dir /root/orch-production-recovery
~~~

Doctor and dry-run do not create credentials, reserve paths, start services, or
change firewall and DNS configuration. Installation creates the non-login
`orch-controller` account, root-owned binary at
`/opt/orch-controller/bin/orch`, private state at `/var/lib/orch-controller`,
and `orch-controller.service`. These paths are separate from the guided worker
bundle, so both roles can coexist on one supported host.

The administrator directory contains `context.json` and its relative identity
tree. Transfer the entire directory over a protected channel, remove the host
copy after verifying the transfer, and import it on the operator machine:

~~~console
orch target import production /secure/orch-production-admin/context.json --use
orch status
~~~

The recovery directory contains a verified standalone database snapshot,
controller PKI, secret master key, and hash manifest. Transfer it off-host
immediately and protect it like administrator credentials. It is a day-zero
snapshot; create regular later backups with `orch cluster backup`. A copy left
only on the controller does not protect against host loss.

Matching reruns preserve the cluster identity and do not restart a healthy
service. An interrupted run resumes its journal. Changed endpoint, listen
address, executable, output paths, or bundle is rejected because upgrades and
reconfiguration are separate operations. Diagnose with:

~~~console
sudo /opt/orch-controller/bin/orch cluster doctor \
  --server https://10.0.0.10:9443 --listen 0.0.0.0:9443 --json
sudo systemctl status orch-controller
sudo journalctl -u orch-controller --since '-10 min'
~~~

Allow the chosen TCP port from trusted workers and operators. The installer
does not change UFW, firewalld, provider firewalls, DNS, routing, or NAT. Fedora
must remain SELinux enforcing and retains operator-managed firewalld rules.

## Guided worker installation

The guided installer supports **Ubuntu 24.04 LTS amd64** and **Fedora Server 44
amd64** on a fresh VM with systemd, cgroups v2, overlayfs and kernel WireGuard.
Fedora must run with SELinux enforcing. The installer copies the executable you
run, so use a trusted Linux amd64 build owned by root and not writable by other
users. See [the bundle](../packaging/node/README.md) for exact dependency pins.

On your administrator machine, create one short-lived artifact per node:

```sh
orch node join create worker-1 --ttl 1h --output worker-1.join
```

Transfer the executable and private artifact over SSH. Keep the artifact mode
0600. On the node, choose an assigned private IPv4 address reachable by its peers:

```sh
sudo ./orch node doctor --advertise-address 10.0.0.11
sudo ./orch node install --advertise-address 10.0.0.11 --dry-run --json
sudo ./orch node install --advertise-address 10.0.0.11 \
  --join-file /root/worker-1.join --wait-timeout 5m
```

`--join-file -` accepts the artifact from stdin. Do not put its contents into
command arguments, environment variables or logs. Doctor and dry-run never read
the artifact, contact a selected target, download dependencies, or change host
state. `--context` and `--target` are rejected for these local commands.

Doctor exits nonzero for failed or unknown required checks. A fresh host normally
reports installable missing dependencies. Dry-run exits successfully when its
remaining failures can be supplied by installation; unsupported platforms,
foreign state, insufficient disk, or a wrong address remain blockers. JSON output
includes each check's state and remedy, the artifact hashes, packages, owned paths
and planned actions. Warnings call out checks that need operator verification.

Installation uses apt on Ubuntu and dnf on Fedora without a blanket host upgrade.
It creates `/opt/orch`, `/var/lib/orch-install`, `/var/lib/orch-containerd`,
`/var/lib/orch-agent`, `/var/lib/orch-ingress`, two systemd units, and owned
sysctl/modules-load files. It enables forwarding and WireGuard, supplies CNI and
Caddy, and creates the non-login `orch-ingress` account. Containerd listens on
`/run/orch-containerd/containerd.sock`. The root agent supervises Caddy under that
separate account; no Caddy service is created.

Host and provider firewall rules remain unchanged. Permit UDP 51820 between the
private node addresses. Ingress nodes need TCP 80/443 from intended clients.
On Fedora, select appropriate firewalld zones and forwarding policies for your
interfaces, mesh and workload traffic; merely opening a port does not permit
forwarded traffic. Keep unrelated nftables tables and existing policies intact.
The default workload CIDR is 10.88.0.0/16; use `orch network configure` before
submitting workloads if that overlaps your network.

A successful command means the agent authenticated, received an assignment, and
reported ready networking/DNS (plus ingress if enabled). It does not mean a
particular application has deployed. Use `orch nodes`, then deploy and watch a
service from your administrator machine. Enable ingress explicitly with
`orch node ingress worker-1 --enable`.

## Retry and diagnosis

After an interruption, rerun the same executable with the same address and join
artifact. The journal resumes completed phases and preserves the enrollment key
and identity. A completed installation can omit `--join-file`; supplying the
original expired artifact is also allowed. A different join artifact, address,
executable, bundle, or modified owned file is rejected. An expired credential
cannot start a new enrollment. Healthy services are not restarted by an identical
rerun, and reboot reuses the existing identity and durable workload state.

A readiness timeout leaves the installation intact. Inspect:

```sh
sudo /opt/orch/bin/orch node doctor --advertise-address 10.0.0.11 --json
sudo systemctl status orch-containerd orch-agent
sudo journalctl -u orch-containerd -u orch-agent --since '-10 min'
# Fedora:
sudo ausearch -m AVC,USER_AVC -ts recent
```

Resolve the reported network, controller or runtime issue and rerun install to
wait again. The private `/var/lib/orch-agent/status.json` receipt is informational,
not a credential or a replacement for controller status. Installation errors do
not delete agent identities, assignment caches, volumes, container state, or
unrelated services. Upgrades, uninstall and adopting manually enrolled nodes are
not supported by this command.

## Disposable VM acceptance drill

Provision one fresh VM per supported distribution with root SSH access, an
assigned private address, working peer/forwarding rules, and a persistent test
controller that remains reachable after the workers reboot. Fedora Server guest
images may require completing their initial setup; do not assume cloud-init is
included. Build the local CLI and Linux amd64 executable, select the test target,
then run the opt-in drill (it reboots both supplied hosts):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/orch-linux ./cmd/orch
python3 scripts/node-install-e2e.py --orch ./bin/orch \
  --linux-binary /tmp/orch-linux --ssh-config /path/to/test-ssh-config \
  --ubuntu ubuntu-test 10.0.0.11 --fedora fedora-test 10.0.0.12 \
  --evidence /tmp/orch-install-evidence --reboot-disposable-vms
```

The drill checks fresh-host doctor/dry-run output, enrollment, PID-preserving
reruns, a health-checked service and completed job on each OS, reboot recovery
with unchanged node IDs and job completion timestamps, and enforcing SELinux
with active firewalld. It records AVC diagnostics for review. Separately test
your ingress domains/TLS and firewall policies against your actual topology.
Evidence includes private join artifacts: publish only reviewed, non-secret
results. The drill deliberately leaves the VMs and test application available
for inspection; destroy your disposable infrastructure afterward.

Fedora's owned `/etc/systemd/network/10-orch-veth.link` preserves the MAC
addresses CNI records for kernel-created `veth*` devices (driver `veth`). Without
it, udev's persistent-MAC policy can change an address after CNI ADD and make
CNI CHECK reject the attachment. The rule applies to future matching veth pairs,
including those created by other software; it does not match physical NICs or
trigger changes to existing interfaces. Installation reloads udev configuration,
not NetworkManager or firewall rules.

## Disposable controller VM acceptance drill

The controller drill requires two fresh disposable amd64 VMs, root SSH aliases,
the Linux amd64 orch and Pkl executables, and addresses assigned to the guests.
It installs a controller and co-located worker on each host, deploys a service,
runs a job, interrupts the controller, validates recovery material, and reboots
both machines:

```sh
python3 scripts/controller-install-e2e.py \
  --linux-binary /tmp/orch-linux --linux-pkl /tmp/pkl-linux-amd64 \
  --ssh-config /path/to/test-ssh-config \
  --ubuntu ubuntu-test 10.0.0.10 9443 \
  --fedora fedora-test 10.0.0.20 9443 \
  --evidence /tmp/orch-controller-evidence --reboot-disposable-vms
```

The evidence directory contains copied administrator credentials and must remain
private. Review only non-secret results before attaching evidence, and destroy
the disposable hosts afterward.
