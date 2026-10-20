# orch panel

`orch panel` is a dashboard you host yourself that manages any number of orch
clusters from one place. Use it when you run more than one cluster, or want
to check on your clusters from any browser without keeping `orch web` running
on your workstation.

The panel shows the same pages as [`orch web`](targets.md) for each connected
cluster: applications, deployment history and diffs, logs, nodes, jobs and
events, with rollback and node actions. A switcher at the top of the sidebar
picks the cluster. There are no passwords. Like `orch web`, you sign in with
a link that works once.

## Run the panel

The panel is one container, `ghcr.io/torstendittmann/orch-panel`, that keeps
its connected clusters in `/data`. Run it wherever you like.

### On one of your orch clusters

Point a DNS name at an ingress node, then run, from a workstation with that
cluster selected as the target:

```console
$ orch panel install --domain panel.example.com
Watching orch-panel generation 1 (rev_…)
…
orch panel is deployed as the application orch-panel.
Connected this cluster to orch panel at https://panel.example.com as production.
Open https://panel.example.com/login?name=orch-panel-login-…&token=…
(the link works once, within 15m0s)
```

`install` deploys the panel as the application `orch-panel`, with automatic
HTTPS and a local volume for its data, connects the cluster it runs on, and
signs your browser in.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--domain` | required | Domain the panel is served on |
| `--image` | the release matching the CLI | Panel image, for development builds |
| `--name` | the cluster's name | Name for this cluster in the panel |
| `--no-wait` | `false` | Return once the deployment is accepted, and print the connect command |
| `--no-open` | `false` | Print the sign-in link without opening a browser |
| `--timeout` | `5m` | How long to wait for the rollout and for the domain to answer |

### Anywhere else

On any Docker host:

```console
$ docker run -d --name orch-panel -p 8080:8080 -v orch-panel:/data \
    -e ORCH_PANEL_URL=https://panel.example.com \
    ghcr.io/torstendittmann/orch-panel
$ docker logs orch-panel
orch panel has no clusters yet. Connect the first one from a workstation with:

  orch panel connect https://panel.example.com --code 3f9a…
```

Put the panel behind a proxy that terminates HTTPS. `orch panel serve` runs
the same panel without a container; see the [CLI reference](cli.md#orch-panel).

## Connect clusters

Run the command from the panel's logs, from a workstation with the cluster
you want to connect selected as the target:

```console
$ orch panel connect https://panel.example.com --code 3f9a…
Connected this cluster to orch panel at https://panel.example.com as production.
Open https://panel.example.com/login?name=orch-panel-login-…&token=…
```

`connect` creates an [administrator access token](security.md) named
`orch-panel` on the cluster, valid for one year, and hands it to the panel.
Then it signs your browser in. Nothing is copied by hand.

To connect more clusters, open **Clusters** in the panel and choose
**Connect cluster**. It shows an `orch panel connect` command with a new code;
run it with the next cluster selected. Each code works once, within 15
minutes. The setup code from the logs only works until the first cluster is
connected.

The panel must be able to reach each cluster's controller (port 9443 by
default) at the address the cluster's administrator context uses. Pass
`--name` to choose the cluster's name in the panel.

Connecting a cluster again replaces its token, which also rotates it.
**Disconnect** on the Clusters page forgets a cluster and revokes the panel's
token on it.

## Sign in

```console
orch panel open [URL] [--no-open]
```

prints a new sign-in link and opens it, from a workstation that has
administrator access to any connected cluster. The URL is remembered after
the first `connect` or `open`. Each link is a read-only access token that
expires after 15 minutes; the panel checks it with the cluster that issued it
and revokes it on first use, so a link copied from browser history does
nothing. Unused links are cleaned up the next time you run `orch panel open`.

Sessions last seven days and end when the panel restarts, for example after an
upgrade. Visit `/logout` to sign out.

## Remove

```console
orch panel remove
```

revokes the panel's access tokens on the selected cluster, so the panel can no
longer manage it, and removes the `orch-panel` application if `install`
deployed it there.

## Security

- The browser only ever holds a session cookie (HttpOnly, Secure,
  SameSite=Lax) for the panel. Cluster credentials stay inside the panel, in
  `/data/clusters.json`, readable only by the panel. Protect its volume like
  the administrator tokens it holds.
- Connecting a cluster needs a code that only someone signed in to the panel,
  or someone who can read its logs before the first cluster, can get.
  `connect` refuses plain HTTP except to this machine, since it sends an
  administrator token.
- Only an administrator of a connected cluster can create sign-in links. The
  panel only asks connected clusters about a link, never the address written
  in it.
- The panel's tokens are exchanged for 15-minute administrator certificates
  and renewed automatically, so `orch token revoke orch-panel` on a cluster
  locks the panel out of it within 15 minutes.
- Anyone signed in can do what an administrator can in the dashboard, on every
  connected cluster. Treat sign-in links like passwords.
