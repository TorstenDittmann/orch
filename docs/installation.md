# Guided node installation

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
