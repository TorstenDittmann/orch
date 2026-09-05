# Installation

orch uses one controller and one or more workers. Start with fresh
**Ubuntu 24.04 LTS amd64** or **Fedora Server 44 amd64** hosts that can reach
each other over private IPv4 addresses.

## Requirements

Choose:

- a stable HTTPS address for the controller;
- a private address for each worker;
- a workload CIDR that does not overlap your host networks (the default is
  `10.88.0.0/16`);
- one workstation from which you will operate the cluster.

Allow the controller's TCP port from trusted workers and operators. Allow UDP
51820 between workers. Nodes that receive public web traffic also need TCP 80
and 443.

The installer does not change UFW, firewalld, cloud firewalls, DNS, routing, or
NAT. On Fedora, keep SELinux enforcing and configure the appropriate firewalld
zones and forwarding policy.

## Controller

Copy a trusted Linux amd64 `orch` binary to the controller and run the
preflight check:

```console
sudo ./orch cluster doctor \
  --server https://10.0.0.10:9443 \
  --listen 0.0.0.0:9443
```

Then install:

```console
sudo ./orch cluster install \
  --name production \
  --server https://10.0.0.10:9443 \
  --listen 0.0.0.0:9443 \
  --admin-dir /root/orch-production-admin \
  --recovery-dir /root/orch-production-recovery
```

Copy both output directories off the controller through a protected channel.
The admin directory lets you operate the cluster. The recovery directory
contains the material needed to recover the controller after host loss.

Import the administrator context on your workstation:

```console
orch target import production ./orch-production-admin/context.json --use
orch target test
```

## Workers

Create a short-lived join file on your workstation:

```console
orch node join create worker-1 --ttl 1h --output worker-1.join
```

Copy the trusted `orch` binary and the join file to the worker. Check the host,
then install the worker with its private address:

```console
sudo ./orch node doctor --advertise-address 10.0.0.11
sudo ./orch node install \
  --advertise-address 10.0.0.11 \
  --join-file ./worker-1.join \
  --wait-timeout 5m
```

Confirm that it joined:

```console
orch nodes
```

Enable public ingress on a node only when you want that node to accept external
traffic:

```console
orch node ingress worker-1 --enable
```

You can now [deploy your first application](quick-start.md).

## Troubleshooting

An interrupted installation can be resumed by running the same command with the
same binary, addresses, and join file. A readiness timeout leaves the installed
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
