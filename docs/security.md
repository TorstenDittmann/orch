# Security and recovery

Protect access to the cluster, store application secrets, and keep recoverable
backups.

## Administrator access

The controller serves its administrator API on two listeners:

- A local socket, `/var/lib/orch-controller/admin.sock`, that only root and the
  controller account can open. `orch` on the controller host uses it when no
  target is selected, and `orch target add NAME ssh://root@HOST` reaches it
  over SSH. Whoever can log in to the controller as root can administer the
  cluster, which they could already do by reading its state.
- The HTTPS port, which requires an mTLS administrator certificate. `orch init`
  leaves one in `/etc/orch/admin` on the controller for break-glass use;
  `orch cluster install --admin-dir ...` writes it where you choose.

Socket targets store no credentials on your workstation; access follows your
SSH keys. For a certificate context, transfer the whole directory through a
protected channel, restrict access to it, remove unnecessary copies, and use
named targets instead of copying certificate paths into scripts:

```console
orch target import production ./orch-production-admin/context.json --use
orch target test
```

Anyone with this context can administer the cluster.

## Certificate renewal

The controller and administrator certificates are valid for one year; the
cluster CA is valid for ten. Worker certificates renew automatically, but the
controller and administrator certificates do not. `orch status` shows both
expiry times, and `orch cluster doctor` warns 30 days before the controller
certificate expires.

Renew both on the controller host. This works before or after the
certificates expire, as long as the cluster CA is still valid. The CA does not
change, so workers and existing backups are unaffected:

```console
sudo orch cluster certificates renew \
  --data-dir /var/lib/orch-controller \
  --admin-dir /root/orch-production-admin-2027
sudo systemctl restart orch-controller
```

The controller loads its new certificate only when it restarts. `--admin-dir`
must be a new directory. It receives a fresh administrator context that you
handle like the one from installation. Replace the credentials of an existing
target and keep its transport settings:

```console
orch target import production ./orch-production-admin-2027/context.json --replace
```

SSH targets reach the controller's local socket and need no change after a
renewal. Old administrator certificates stay valid until they expire. After renewing,
refresh the `pki` copy in your off-host backups. If a restore finds an expired
controller certificate, it reports that and still restores the data. Run the
renewal against the restored data directory before starting the controller.

## Worker enrollment

Create one join token per worker:

```console
orch join-token worker-1
```

A token admits one node with that name, once, until it expires (one hour by
default). It also pins the cluster CA fingerprint, so `orch join` only enrolls
with your controller. A used or expired token is worthless, but an unused one
admits a node: send it through a channel you trust. While `orch join` runs, the
token is visible in the worker's process list; pass it on stdin with
`orch join -` if other users share that host. Revoke a token you no longer
need with `orch node join revoke`.

Join files from `orch node join create` carry the same credential. Transfer
them privately, keep them mode 0600 and delete copies after the worker has
joined.

## Secrets

Send values through stdin:

```console
printf '%s' "$DATABASE_URL" | orch secret set hello database-url \
  --from-stdin --target production
```

Application files reference secret names and can remain in source control.
Secret values are encrypted by the controller and delivered only to assigned
workers. Files are projected below `/run/secrets`; environment values are
provided only to the workload that references them.

Rotate a secret by setting the same name again. The controller reconciles right
away and replaces the services and cron jobs that reference it; no deploy is
needed. Remove unused values with `orch secret delete`. See
[Secrets](secrets.md) for details.

## Backups

`orch init --recovery-dir DIR` and `orch cluster install` can write a day-zero
recovery directory. It is only a snapshot. Create newer backups after configuration changes and on a schedule:

```console
sudo orch cluster backup \
  --data-dir /var/lib/orch-controller \
  --output /secure/orch-backups/production.db \
  --secret-key-output /secure/orch-backups/production.key
```

Copy each completed backup off the controller. Protect it like administrator
credentials because it includes:

- the controller database backup;
- the separately written secret-encryption key.

Also preserve the controller's `pki` directory with an access-controlled
system backup. The database backup does not contain private keys, administrator
contexts, worker identities, workload logs, or local-volume contents.

A backup stored only on the controller does not protect against host loss.

## Recovery

Use a fresh supported host and an off-host database backup, secret key, and PKI
directory. Inspect the backup before restoring it:

```console
orch cluster backup inspect \
  --backup /secure/orch-backups/production.db \
  --pki-dir /secure/orch-backups/pki \
  --secret-key /secure/orch-backups/production.key \
  --json

sudo ./orch cluster restore \
  --backup /secure/orch-backups/production.db \
  --pki-dir /secure/orch-backups/pki \
  --secret-key /secure/orch-backups/production.key \
  --data-dir /var/lib/orch-recovered

sudo ./orch controller serve \
  --data-dir /var/lib/orch-recovered \
  --listen 0.0.0.0:9443
```

Restore the same canonical controller HTTPS address. Existing workers keep
their assigned workloads running during the outage and reconnect when the
controller returns.

After recovery:

```console
orch target test production
orch nodes --target production
orch status --target production
```

Confirm node identity, application state, secrets, ingress, and a new backup
before resuming changes.

## Security model

Controller, worker, and operator traffic uses mutually authenticated TLS.
Installation creates separate credentials for each role.

- The controller is a single administrative and scheduling authority.
- Workers keep existing workloads running during a controller outage.
- New scheduling and administrative changes require the controller.
- orch does not provide automatic fencing or a highly available control plane.
- Local volumes remain on one worker and need an application-level backup plan.
- Host firewalls, operating-system updates, DNS, and off-host backup retention
  remain operator responsibilities.

See [limits and expectations](product.md) before using orch for a workload with
strong availability or storage requirements.
