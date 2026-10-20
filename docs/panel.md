# orch panel

`orch panel` runs the web dashboard on your cluster itself: always on, at your
own domain. Use it when you want to check on the cluster from any browser
without keeping `orch web` running on your workstation.

The panel is an ordinary orch application named `orch-panel`. It shows the
same pages as [`orch web`](targets.md): applications, deployment history and
diffs, logs, nodes, jobs and events, with rollback and node actions. There are
no passwords. Like `orch web`, you sign in with a link that works once.

## Install

Point a DNS name at an ingress node, then run, from a workstation with a
selected target:

```console
$ orch panel install --domain panel.example.com
Watching orch-panel generation 1 (rev_…)
…
orch panel is deployed as the application orch-panel. Point panel.example.com at an ingress node.
Open https://panel.example.com/login?name=orch-panel-login-…&token=…
(the link works once, within 15m0s)
```

The link opens in your browser and signs it in for seven days.

`install` does three things:

1. Creates an [administrator access token](security.md) named `orch-panel`
   that the panel uses to reach the controller, valid for one year.
2. Stores it as a secret of `orch-panel`.
3. Deploys `ghcr.io/torstendittmann/orch-panel` in the version of your orch
   CLI, with automatic HTTPS for the domain.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--domain` | required | Domain the panel is served on |
| `--image` | the release matching the CLI | Panel image, for development builds |
| `--no-wait` | `false` | Return once the deployment is accepted |
| `--no-open` | `false` | Print the sign-in link without opening a browser |
| `--timeout` | `5m` | Rollout wait deadline |

Running `install` again with the same domain rotates the panel's token.

## Sign in

```console
orch panel open [--no-open]
```

prints a new sign-in link and opens it. Each link is a read-only access token
for `orch-panel` that expires after 15 minutes; the panel checks it with the
controller and revokes it on first use, so a link copied from browser history
does nothing. Unused links are cleaned up the next time you run
`orch panel open`.

Sessions last seven days and end when the panel restarts, for example after an
upgrade. Visit `/logout` to sign out.

## Remove

```console
orch panel remove
```

revokes the panel's access tokens and removes the `orch-panel` application.

## Security

- The browser only ever holds a session cookie (HttpOnly, Secure,
  SameSite=Lax) for the panel. Cluster credentials stay inside the panel.
- Only an administrator can create sign-in links. The panel only asks its own
  controller about a link, never the address written in it.
- The panel's token is exchanged for 15-minute administrator certificates
  and renewed automatically, so `orch token revoke orch-panel` locks the panel
  out within 15 minutes.
- Anyone signed in can do what an administrator can in the dashboard. Treat
  sign-in links like passwords.
