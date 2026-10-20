# orch panel

`orch panel` runs the web dashboard on your cluster itself: always on, at your
own domain, behind a password. Use it when you want to check on the cluster
from any browser, without installing orch or keeping `orch web` running on
your workstation.

The panel is an ordinary orch application named `orch-panel`. It shows the
same pages as [`orch web`](targets.md): applications, deployment history and
diffs, logs, nodes, jobs and events, with rollback and node actions.

## Install

Point a DNS name at an ingress node, then run, from a workstation with a
selected target:

```console
$ orch panel install --domain panel.example.com
Watching orch-panel generation 1 (rev_…)
…
orch panel is deployed as the application orch-panel.
Point panel.example.com at an ingress node, then set the administrator password at:

  https://panel.example.com/setup?token=…

The link works once. Run orch panel install again to reset the password or rotate the panel's access token.
```

Open the link and choose a password of at least 12 characters. That signs
you in; later visits ask for the password.

`install` does three things:

1. Creates an [administrator access token](security.md) named `orch-panel`
   that the panel uses to reach the controller, valid for one year.
2. Stores it and the one-time setup token as secrets of `orch-panel`.
3. Deploys `ghcr.io/torstendittmann/orch-panel` in the version of your orch
   CLI, with automatic HTTPS for the domain and a `data` volume for the
   password, backed up nightly.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--domain` | required | Domain the panel is served on |
| `--image` | the release matching the CLI | Panel image, for development builds |
| `--no-wait` | `false` | Return once the deployment is accepted |
| `--timeout` | `5m` | Rollout wait deadline |

## Reset the password or rotate the token

Run `orch panel install` again with the same domain. It replaces the access
token and prints a new setup link. Setting a new password signs out every
browser.

## Remove

```console
orch panel remove
```

revokes the panel's access token and removes the `orch-panel` application.

## Security

- The browser only ever holds a session cookie (HttpOnly, Secure,
  SameSite=Lax) for the panel. Cluster credentials stay inside the panel.
- The panel's token is exchanged for 15-minute administrator certificates
  and renewed automatically, so `orch token revoke orch-panel` locks it out
  within 15 minutes.
- Passwords are stored as PBKDF2-SHA256 hashes in the panel's volume. After 10
  failed sign-ins in a minute, the panel refuses further attempts for the rest
  of that minute.
- Anyone who can sign in can do what an administrator can in the dashboard.
  Choose a strong password.
