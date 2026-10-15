# Secrets and registry credentials

orch stores two kinds of sensitive values for you: application secrets, such
as database passwords and API tokens, and credentials for private OCI image
registries. Both are write-only. You send a value once through stdin, and orch
encrypts it, delivers it only to the workers that need it, and never shows it
again.

Application manifests contain only secret names, so `orch.pkl` can stay in
source control.

## Application secrets

A secret belongs to one application and has a name that follows the DNS label
rule: lowercase letters, digits and `-`, starting with a letter, at most 63
characters.

### Create a secret

`orch secret set` reads the value from stdin. `--from-stdin` is required, so a
value never appears in your shell history or the process list as an argument:

```console
printf '%s' "$DATABASE_URL" | orch secret set shop database-url \
  --from-stdin --target production
```

```text
Stored secret shop/database-url version secv_...
```

Use `printf '%s'` rather than `echo`, which adds a trailing newline that would
become part of the value. Values must be between 1 byte and 1 MiB.

Files work the same way:

```console
orch secret set shop tls-key --from-stdin --target production < ./tls.key
```

You can set secrets before the application is deployed for the first time.
That is the recommended order, because a deployment that references a missing
secret is blocked.

### Use a secret

Reference secrets from a service or cron job in `orch.pkl`, either as
environment variables or as files:

```pkl
services {
  ["web"] {
    image = "ghcr.io/acme/shop:2.3.0"
    secretEnv {
      ["DATABASE_URL"] = "database-url"
      ["STRIPE_API_KEY"] = "stripe-api-key"
    }
    secretFiles {
      new SecretFile {
        source = "tls-key"
        target = "/run/secrets/tls.key"
      }
    }
  }
}
```

- `secretEnv` maps an environment variable name to a secret name. The same
  variable cannot also be set in `env`.
- `secretFiles` projects a secret as a read-only file. `target` must be a
  clean absolute path below `/run/secrets/`, and each target can be used once.

The same secret can be referenced by several services and cron jobs of the
application, and both as a variable and as a file. One-off jobs and pre-deploy
hooks receive the secrets of the service they run as. See the
[manifest reference](manifest.md) for every rule.

A value used as an environment variable must be valid UTF-8 and contain no NUL
bytes. Binary data, such as a keystore, must be projected as a file. Otherwise
the application is blocked with `secret_environment_invalid`.

### List secrets

```console
orch secret list shop --target production
```

```text
database-url  secv_...  referenced=true  updated=2026-10-12T09:14:03.51Z
old-token  secv_...  referenced=false  updated=2026-07-01T16:40:11.87Z
stripe-api-key  secv_...  referenced=true  updated=2026-10-12T09:14:20.02Z
```

Each line shows the name, the current version, whether the current
application or a running workload references it, and when it last changed.
Values are never returned. Add `--json` for machine-readable output.

### Rotate a secret

Set the same name again:

```console
printf '%s' "$NEW_DATABASE_URL" | orch secret set shop database-url \
  --from-stdin --target production
```

Every `secret set` creates a new immutable version, even when the value is
unchanged. The controller reconciles immediately. Services and cron jobs that
reference the secret are replaced so they receive the new version; workloads
that do not reference it keep running untouched. You do not need to run
`orch deploy`.

A process reads its environment once, at startup, so restarting is the only
way it can see a new value. Plan rotations so that old and new values both
work for a while: for example, add a new database password, rotate the secret,
wait for `orch app status` to report the application healthy, then revoke the
old password.

Earlier versions are kept, encrypted, so that a rollback to an older revision
can still be authorized.

### Delete a secret

```console
orch secret delete shop old-token --target production
```

orch refuses to delete a secret that the current application or a running
workload still references. Remove the reference from `orch.pkl`, deploy, and
then delete the secret.

### Missing secrets

If a deployment references a secret that does not exist, the controller does
not start the new revision. `orch app status` reports the application as
`BLOCKED` with the reason `secret_missing` and names the secret. Workloads from
the previous revision keep running. Set the secret and the deployment
continues on its own.

## Registry credentials

Registry credentials are cluster-wide. Each one is stored for a registry host,
optionally with a port, and is used for every image on exactly that host.

### Log in

```console
printf '%s' "$REGISTRY_TOKEN" | orch registry login ghcr.io \
  --username robot --password-stdin --target production
```

```text
Stored registry credential for ghcr.io as robot (version regv_...)
```

`--username` and `--password-stdin` are required. The password or token is
read from stdin and must be between 1 byte and 1 MiB.

For a registry on a custom port, include the port in the host:

```console
printf '%s' "$REGISTRY_TOKEN" | orch registry login registry.example:5443 \
  --username robot --password-stdin --target production
```

### How the host is matched

orch takes the registry host from each image reference and looks for a
credential with exactly that host:

| Image | Registry host |
| --- | --- |
| `ghcr.io/acme/shop:2.3.0` | `ghcr.io` |
| `registry.example:5443/team/api:1.0` | `registry.example:5443` |
| `postgres:17` | `docker.io` |
| `acme/tools:1.0` | `docker.io` |

Images without a matching credential are pulled anonymously.

The credential is used in two places:

- By the controller, when it resolves an image tag to a digest at deployment
  time. Log in before you deploy an image from a private registry, or the
  deployment is rejected.
- By each worker, when it pulls the image.

Registries must serve HTTPS with a certificate that the system trust store
accepts. Plain-HTTP registries, custom certificate authorities and registry
mirrors are not supported.

### List and rotate

```console
orch registry list --target production
```

```text
ghcr.io  robot  regv_...
registry.example:5443  robot  regv_...
```

To rotate a token, run `orch registry login` again for the same host. Unlike
application secrets, a new registry credential does not restart running
workloads; it is used for the next tag resolution and the next image pull.

### Log out

```console
orch registry logout registry.example:5443 --target production
```

Later pulls from that host are anonymous.

## How values are protected

| Stage | Protection |
| --- | --- |
| On your workstation | The CLI reads the value from stdin, sends it over the authenticated administrator connection, and wipes its copy |
| On the controller | Each version is encrypted with AES-256-GCM using its own data key, which is wrapped with the controller's master key |
| In transit to workers | Values are sent over the mutually authenticated agent connection only to workers that run a workload referencing them |
| On workers | The agent encrypts values again with its own cache key before storing them, so a worker can restart while the controller is unavailable |
| In the container | Secret files are read-only, mode 0400, owned by the container's user, and backed by a root-only tmpfs |

The controller's master key is the file `secret-master.key` in its data
directory, `/var/lib/orch-controller` by default. It is kept outside the
database and must have mode 0600. Each worker keeps its own
`secret-cache.key` in its data directory. Once the controller has stored a
master key, it refuses to start if the key file is missing or does not match
the database, instead of silently creating a new key. Protect the file and
include it in your backups as described below.

Environment variables from `secretEnv` are not written into the stored
container specification. A small initializer inside the container reads them
from the tmpfs and then replaces itself with your process. Once your process
runs, the values are part of its environment like any other variable, so avoid
logging the environment or exposing it through debug endpoints.

The administrator API never returns plaintext. List commands show names,
hosts, usernames and version identifiers only, and job details never include
environment values or secrets.

## Backups

The controller database contains encrypted secrets but not the master key.
`orch cluster backup` therefore refuses to run without `--secret-key-output`
once the cluster has a master key or stored secrets:

```console
sudo /opt/orch-controller/bin/orch cluster backup \
  --data-dir /var/lib/orch-controller \
  --output /secure/orch-backups/production.db \
  --secret-key-output /secure/orch-backups/production.key
```

Store the two files separately. The database backup alone cannot decrypt any
secret, and losing the key means every secret and registry credential must be
set again after a restore. See [backups](security.md#backups) and
[recovery](security.md#recovery).

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `secret set requires APP NAME, --from-stdin, and --context` | Add `--from-stdin` and pipe the value in. Select a target with `orch target use` or `--target`. |
| `secret stdin must contain 1-1048576 bytes` | Nothing was piped in, or the value is larger than 1 MiB |
| Application `BLOCKED` with `secret_missing` | Set the named secret with `orch secret set` |
| Application `BLOCKED` with `secret_environment_invalid` | The value is not valid UTF-8 or contains a NUL byte; use `secretFiles` instead |
| `secret delete` fails because the secret is referenced | Remove the reference, deploy, then delete |
| Deployment rejected while resolving an image tag | Log in to the registry with `orch registry login`, or check the host and port in the image name |
| The process still sees the old value | Check `orch app status`; the replacement may still be waiting for capacity or readiness |

See [security and recovery](security.md) for the wider security model.
