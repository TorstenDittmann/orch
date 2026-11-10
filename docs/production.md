# Production checklist

orch runs a production cluster with a few one-time steps. Most of them take a
single command. Do them before the cluster carries traffic you care about.

## Before you start

- **Install from a signed release.** `install.sh` and `install.ps1` check the
  release signature and checksums; see
  [release signatures](security.md#release-signatures). Workers upgraded by
  `orch cluster upgrade` are checked the same way.
- **Pin your manifests to the v1 schema.** Start each `app.pkl` with
  `amends "orch:/v1/Application.pkl"`. [Compatibility](compatibility.md)
  lists what stays stable across 1.x.
- **Check each host.** `orch node doctor` reports anything the installer
  could not fix by itself.

## Backups

- **Send backups off the controller.** A backup on the controller does not
  survive losing that host:

  ```console
  orch backup destination set s3://acme-backups/production
  ```

  See [off-host backups](operations.md#off-host-backups).
- **Store the recovery key.** The first `set` prints it once. Put it in a
  password manager; restoring the controller from the bucket needs it.
- **Limit who can write to the bucket.** Restoring a backup restores the
  cluster CA and every secret, so anyone who can plant a backup there can
  take over the cluster when you restore it. Give the access key orch uses
  its own bucket or prefix, and do not share write access with other systems.
- **Back up stateful volumes.** Give each volume that holds data a `backup`
  block; see [volume backups](operations.md#volume-backups).
- **Practise a restore.** Restore the newest backup into a scratch directory
  once, and again after major changes, so the first restore is not during an
  outage:

  ```console
  sudo -E orch cluster restore --from s3://acme-backups/production \
    --data-dir /tmp/orch-restore-drill
  ```

  The command checks the backup, the recovery key and the PKI before it
  writes anything. Delete the drill directory afterwards. After a real
  restore, follow the checks in [backups](operations.md#backups).

## Monitoring and alerts

- **Send cluster alerts somewhere you read.** One command, and it sends a
  test message:

  ```console
  orch notify add slack https://hooks.slack.com/services/...
  ```

  Lost nodes, full disks, expiring certificates and stale backups are then
  reported; see [cluster notifications](notifications.md#cluster-notifications).
- **Scrape `/metrics`** with Prometheus if you run it; see
  [monitoring](operations.md#monitoring).

## Keeping it current

- **Patch the hosts.** Run `orch node update --all` on a schedule, monthly or
  when a `node.reboot_required` alert arrives. It drains, upgrades and reboots
  one node at a time; see
  [operating-system updates](operations.md#operating-system-updates).
- **Upgrade orch.** `orch cluster upgrade` upgrades the controller and then
  every worker; see [upgrades](operations.md#upgrades). It also refreshes the
  bundled containerd.
- **Certificates renew themselves.** Workers, the controller and administrator
  contexts in regular use renew ahead of expiry. A `certificate.expiring`
  alert means renewal is failing; see
  [certificate renewal](security.md#certificate-renewal).

## Capacity

- **Run at least two workers** for anything that must stay up while a node is
  drained or updated, and give such services `replicas = 2` or more.
- **Know the limits.** [Limitations](product.md) lists what one controller
  supports and what orch leaves to you.
