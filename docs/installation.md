# Installation

This guide installs a controller and adds workers to the cluster.

## Requirements

| Requirement | Supported setup |
| --- | --- |
| Hosts | Ubuntu 24.04 LTS or Fedora 44 (any edition except ostree-based ones) on amd64 |
| Network | IPv4 connectivity between hosts, over private or public addresses |
| Controller | A stable HTTPS address |
| Workstation | A machine for running the orch CLI |

The default workload network is `10.88.0.0/16`. Choose a different range if it
overlaps an existing network.

Allow the controller's TCP port from trusted workers and operators. Allow UDP
51820 between workers. When workers use public addresses, allow UDP 51820
only from the other workers' addresses. Nodes that receive public web traffic also need TCP 80
and 443.

The installer does not change UFW, firewalld, cloud firewalls, DNS, routing, or
NAT, and it never changes the SELinux mode. On Fedora it installs SELinux file
labels whenever SELinux is enabled; doctor warns when SELinux is permissive or
disabled. Configure the appropriate firewalld zones and forwarding policy.

## Controller

Download the Linux amd64 `orch` binary and its checksum from the
[latest release](https://github.com/TorstenDittmann/orch/releases/latest), verify
it, and copy it to the controller:

```console
curl -fLO https://github.com/TorstenDittmann/orch/releases/latest/download/orch-linux-amd64
curl -fLO https://github.com/TorstenDittmann/orch/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS
install -m 0755 orch-linux-amd64 orch
```

Then run, as root:

```console
sudo ./orch init
```

`orch init` checks the host, installs the controller as a systemd service and
waits until it answers. If the host cannot be installed, it lists each blocking
check with its fix instead. It detects the host's address and listens on port
9443; pass `--advertise-address`, `--port` or `--name` to override them. Use
the address workers and your workstation can reach. Rerunning the same command
resumes an interrupted installation.

On the controller host, root can now run any `orch` command without further
setup, for example `sudo ./orch nodes` with the executable you copied. The CLI talks to the controller's local
administrator socket, which only root can open.

To manage the cluster from your workstation, add a target that reaches that
socket over SSH. Your SSH key for `root` on the controller is the credential:

```console
orch target add production ssh://root@10.0.0.10
orch target test
```

`orch init` keeps a break-glass mTLS administrator context in
`/etc/orch/admin` on the controller. You do not need it for normal use. Use
`orch cluster install` instead when you want to choose where the administrator
and recovery directories are written.

## Workers

Create a join token for each worker, on the controller host or from your
workstation:

```console
orch join-token worker-1
```

It prints a command. Copy the trusted `orch` binary to the worker and run that
command there as root:

```console
sudo ./orch join orch1.eyJlIjoiaHR0cHM6Ly8xMC4wLjAuMTA6OTQ0MyIs...
```

The token is valid once, for one hour by default (`--ttl` accepts 1m to 24h),
and pins the cluster CA, so the worker refuses a controller that is not yours.
`orch join` checks the host the same way, installs the pinned dependencies and
services, and advertises the host's detected address to the other workers. Pass
`--advertise-address` to choose a different one.

Machines without a shared private network can use their public address
instead. See [public advertise addresses](networking.md#public-advertise-addresses)
for the firewall rule that should go with it.

Confirm that it joined:

```console
orch nodes
```

The first node that joins accepts public ingress automatically. Enable ingress
on later nodes when you want them to accept external traffic:

```console
orch node ingress worker-2 --enable
```

### Join files

`orch node join create` and `orch node install --join-file` remain available
when you prefer to transfer a join file:

```console
orch node join create worker-1 --ttl 1h --output worker-1.join
sudo ./orch node doctor --advertise-address 10.0.0.11
sudo ./orch node install --advertise-address 10.0.0.11 --join-file ./worker-1.join
```

You can now [deploy your first application](quick-start.md).

## Troubleshooting

An interrupted installation can be resumed by running the same command with the
same binary, addresses, and join token or file. A readiness timeout leaves the installed
services in place.

On a controller:

```console
sudo systemctl status orch-controller
sudo journalctl -u orch-controller --since '-10 min'
```

On a worker:

```console
sudo /opt/orch/bin/orch node doctor --advertise-address 10.0.0.11 --json
sudo systemctl status orch-containerd orch-agent
sudo journalctl -u orch-containerd -u orch-agent --since '-10 min'
```

On Fedora, inspect recent SELinux denials with
`sudo ausearch -m AVC,USER_AVC -ts recent`.

## Upgrades

Upgrade the controller before workers. Copy the trusted new binary to each host
and inspect the change before applying it:

```console
sudo ./new-orch cluster upgrade --binary ./new-orch --dry-run --json
sudo ./new-orch cluster upgrade --binary ./new-orch

sudo ./new-orch node upgrade --binary ./new-orch --dry-run --json
sudo ./new-orch node upgrade --binary ./new-orch
```

Worker upgrades restart the agent while existing workloads continue under
containerd. Keep the recovery path reported by each upgrade until the cluster
has been observed under normal traffic.
