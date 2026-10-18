# Targets and interfaces

A target is a named, remembered connection to a cluster's controller. You add
it once, and every remote command uses it until you select another. This page
explains how targets reach a controller, where they are stored, and how to use
the two built-in interfaces that run on top of them: the terminal UI
(`orch tui`) and the browser dashboard (`orch web`).

All three ways of working, the CLI, the TUI, and the dashboard, use the same
target and the same administrator access. Pick whichever suits the task:

| Interface | Best for |
| --- | --- |
| CLI commands | Scripts, CI, and anything you want in shell history; every command supports `--json` |
| `orch tui` | Watching a cluster from a terminal, following logs, and quick node actions over SSH sessions |
| `orch web` | Browsing applications, deployment history, diffs, and logs with a mouse on your workstation |
| `orch mcp` | Letting an AI assistant such as Claude Code operate the cluster; see [AI assistants](ai.md) |

## How the CLI picks a cluster

Every command that talks to a controller resolves its cluster in this order:

1. `--target NAME` on the command line.
2. The `ORCH_TARGET` environment variable.
3. The selected target.
4. On the controller host, when no target is selected, the controller's local
   administrator socket.

```console
orch nodes                          # selected target
orch nodes --target staging         # one-off override
ORCH_TARGET=staging orch nodes      # override for a shell or CI job
```

When nothing applies, orch tells you how to fix it:

```text
no target is selected; run 'orch target add NAME ssh://root@CONTROLLER' or 'orch target use NAME'
```

`--context FILE` is still accepted for compatibility. It points at an
administrator context file directly and cannot be combined with `--target`.
Prefer named targets so credential paths stay out of shell history and scripts.
The [CLI reference](cli.md) lists which commands are remote.

## On the controller host

`orch init` installs a controller that serves a local administrator socket at
`/var/lib/orch-controller/admin.sock`. Only root and the controller's own
account can open it. On that host, root can run any command without adding a
target:

```console
sudo orch status
sudo orch nodes
sudo orch join-token worker-1
```

This fallback only applies while no target is selected. Being able to log in
to the controller as root already means being able to read its state, so the
socket needs no further credential. See
[administrator access](security.md#administrator-access).

## SSH targets

The recommended way to manage a cluster from a workstation is an SSH target.
It reaches the same administrator socket through your own OpenSSH client:

```console
$ orch target add production ssh://root@10.0.0.10
Added target production for cluster production
Using target production
```

The location has the form `ssh://[USER@]HOST[:PORT]`. `HOST` may be an alias
from your `~/.ssh/config`, because orch runs the system `ssh` command. Passwords
in the URL are refused; use SSH keys.

When you add the target, orch connects once to confirm that a controller
answers, and records that cluster's ID and name. The first target you add is
selected automatically. Pass `--use` to select a later one immediately.

Your SSH login is the credential. No certificates or keys are copied to your
workstation, so access follows your SSH keys: remove a key from the
controller's `authorized_keys` and that person loses access. Because the socket
requires root, log in as `root` or as a user that can open the socket.

### How the connection works

For each command, orch starts:

```text
ssh -N -T -o BatchMode=yes -o ExitOnForwardFailure=yes -o StreamLocalBindMask=0177 [-p PORT] \
  -L LOCAL_SOCKET:/var/lib/orch-controller/admin.sock -- HOST
```

`LOCAL_SOCKET` lives in a new private temporary directory (mode 0700) on your
workstation, so no other local user can use the forwarded socket. The tunnel is
closed when the command exits.

`BatchMode=yes` means SSH never prompts. Your key must work without a password
prompt, through `ssh-agent` or an unencrypted key, and the controller's host key
must already be known. orch does not change host-key policy. Check with a plain
SSH login first if a target cannot connect:

```console
ssh root@10.0.0.10 true
orch target test production
```

When the controller listens on a different socket path, add it to the URL:

```console
orch target add lab ssh://root@lab.example.com/run/orch/admin.sock
```

If a different cluster answers behind a socket target, for example after the
host was reinstalled, commands that read the cluster identity, such as
`orch target test` and `orch status`, fail with
`controller cluster identity does not match administrator context`. Add a new
target for the new cluster.

### Local socket targets

`orch target add` also accepts `unix:///PATH` for a socket on the machine you
are on. This is useful on the controller host when you want a named target
instead of the implicit fallback:

```console
sudo orch target add local unix:///var/lib/orch-controller/admin.sock
```

## Certificate targets

The controller's HTTPS port accepts administrator certificates over mutual
TLS. `orch init` keeps one such context in `/etc/orch/admin` for break-glass
use, and `orch cluster install --admin-dir DIR` writes one where you choose.
Import a context to make it a target:

```console
orch target import production ./orch-production-admin/context.json --use
orch target test
```

Import checks the context and copies its CA certificate, administrator
certificate, and private key into the target store. The original directory is
no longer needed by orch afterwards; protect or delete it according to your
policy. Anyone with the context can administer the cluster. See
[administrator access](security.md#administrator-access).

After [renewing certificates](security.md#certificate-renewal), swap in the new
credentials and keep the target's transport:

```console
orch target import production ./orch-production-admin-2027/context.json --replace
```

`--replace` only accepts a context for the same cluster, and works even when
the old certificate has already expired.

### Transports

A certificate target reaches the controller in one of three ways. Imported
targets start as `direct`.

| Transport | How it connects | Set with |
| --- | --- | --- |
| `direct` | Dials the controller URL from the context | `orch target transport NAME direct` |
| `wireguard` | Dials a different address that your own WireGuard interface routes to the controller | `orch target transport NAME wireguard --address HOST:PORT` |
| `ssh` | Opens `ssh -N -T -L 127.0.0.1:PORT:DESTINATION` to an SSH host and dials the loopback end | `orch target transport NAME ssh --host SSH_ALIAS [--destination HOST:PORT]` |

```console
orch target transport production wireguard --address 10.200.0.1:9443
orch target transport production ssh --host bastion --destination 10.0.0.10:9443
```

For `ssh`, `--destination` defaults to the host and port of the controller URL,
as seen from the SSH host. orch does not create WireGuard keys or interfaces on
your workstation; the `wireguard` transport only changes the dial address.

Whichever transport you choose, TLS 1.3 uses the canonical controller URL as
the server name and verifies the cluster CA, the controller's role, and the
cluster ID. A transport is only a route to the same authenticated controller,
not a different trust mode.

A socket target (added with `orch target add`) cannot be switched to a
certificate transport, and a certificate target cannot be switched to a socket.
Add a new target instead.

## Managing targets

```console
$ orch target list
* production               socket via root@10.0.0.10
  staging                  direct
$ orch target use staging
Using target staging
$ orch target test
Connected to staging through target staging (direct)
```

`orch target list` marks the selected target with `*` and shows each target's
transport. `orch target test [NAME]` connects within 10 seconds and prints the
cluster name, which makes it a quick health check for a workstation.

### Where targets are stored

Targets live in `orch` under your user configuration directory:
`$XDG_CONFIG_HOME/orch` when `XDG_CONFIG_HOME` is set, otherwise the operating
system's default, such as `~/.config/orch` on Linux or
`~/Library/Application Support/orch` on macOS.

```text
orch/
  current.json                    selected target
  targets/
    production/
      target.json                 transport and, for socket targets, cluster identity
    staging/
      target.json
      context.json                certificate targets only
      identities/CLUSTER_ID/
        ca.pem  admin.pem  admin-key.pem
```

Directories are created with mode 0700 and files with mode 0600. orch refuses
to use a target directory that group or other users can read. Treat the store
like the administrator credentials it contains: back it up, copy it, and revoke
it accordingly.

Target names start with a lowercase letter and contain up to 63 lowercase
letters, digits, and hyphens. orch has no command to delete a target; to forget
one, select another target and remove its directory under `targets/`.

### New clusters

`orch init` prints the `orch target add` command to run on your workstation.
`orch cluster init` without `--context` stores the new cluster's administrator
credentials as a target named after the cluster and selects it.

## The terminal UI

`orch tui` opens a full-screen, keyboard-driven view of the selected target. It
is part of the `orch` binary and works through any target, including SSH
targets.

```console
orch tui
orch tui --target production --refresh 5s
```

`--refresh` sets how often the cluster snapshot is reloaded, from `1s` to
`1m` (default `2s`). The TUI needs an interactive terminal; for automation use
the regular commands with `--json`.

The header shows the target and its transport. When a refresh fails, the last
data stays on screen and is marked degraded instead of disappearing.

### Views

| Key | View | Shows |
| --- | --- | --- |
| `1` | Overview | Cluster health and counts of healthy, progressing, and failed applications |
| `2` | Applications | Applications and their status; `enter` opens their runtimes, `d` their deployments, `o` their jobs |
| `3` | Nodes | Nodes with lifecycle, connection, and ingress; `enter` opens the runtimes on that node |
| `4` | Events | Recent cluster events, with a filter |
| `5` | Runtimes | Service replicas and cron runs, grouped by application or node, with live logs |
| `6` | Help | The keyboard guide |
| `7` | Deployments | History of one application, revision diffs, and rollback |
| `8` | Jobs | One-off jobs, for one application when opened with `o`, with logs, cancel, and retry |

`tab` and `shift+tab` cycle through the views.

### Keys

| Key | Action |
| --- | --- |
| `↑` `↓` or `k` `j` | Move the selection |
| `enter` | Open the selected item |
| `esc` | Go back |
| `ctrl+p` or `:` | Command palette |
| `/` | Filter events, runtimes, or log text; elsewhere opens the palette |
| `r` | Refresh now |
| `?` | Toggle the keyboard guide |
| `q` or `ctrl+c` | Quit |

The command palette fuzzy-matches views, applications, nodes, runtimes, cron
runs, deployment histories, and node actions, so typing a few letters of a name
is usually the fastest way to get anywhere.

### Runtimes and logs

The Runtimes view lists every replica of every service and every retained cron
run that orch manages. Select one and press `enter` to open its log pane. Only
orch-managed workloads appear; other containers on the hosts and secret values
are never shown.

| Key | Action |
| --- | --- |
| `h` / `l` | Switch between the runtime tree and the log pane |
| `g` | Group runtimes by application or by node |
| `s` | Show stdout, stderr, or both |
| `w` | Toggle line wrapping |
| `p` | Pause or resume following |
| `G` or `end` | Jump to live output and resume following |
| `/`, then `n` / `N` | Search log text, then go to the next or previous match |
| `x` | Clear filters |

Logs follow new output while you are at the bottom of the pane. Scrolling up
pauses following. The pane reports when a workload exits, its logs are
truncated or rotated, or it is replaced. The controller does not store
workload logs; they are read from the agent that runs the workload.

### Actions

The TUI can change the cluster in a few guarded ways. Each action first shows
its target and consequence and waits for `y` (or `n` / `esc` to cancel). For
node actions and rollbacks, if the node or application changed in the
meantime, the action is dropped and you are asked to review it again.

| View | Key | Action |
| --- | --- | --- |
| Nodes | `c` | Cordon an active node |
| Nodes | `u` | Uncordon a cordoned node |
| Nodes | `d` | Drain an active or cordoned node |
| Nodes | `i` | Enable or disable ingress |
| Deployments | `enter` | Compare the selected revision with the current one |
| Deployments | `b` | Roll back to the selected older revision |
| Deployments | `n` / `N` | Older page / latest page |
| Jobs | `enter` | Open the job's logs |
| Jobs | `c` | Cancel a running job |
| Jobs | `x` | Retry a finished, unsuccessful job |
| Jobs | `n` / `N` | Older page / latest page |

A rollback restores the selected revision as a new deployment. It does not
restore data volumes or secret values.

## The web dashboard

`orch web` serves a browser dashboard for the selected target from the `orch`
process on your machine:

```console
$ orch web
orch web is serving target production
Open http://127.0.0.1:49731/?token=4c1e... (the link works once)
Press Ctrl+C to stop.
```

orch tries to open the URL in your default browser. Pass `--no-open` to only
print it, and `--port N` to choose the port instead of a free one. The
dashboard runs until you press Ctrl+C.

```console
orch web --target staging --port 7070 --no-open
```

### What it shows

| Page | Contents |
| --- | --- |
| Overview | Cluster capacity and the applications, nodes, and runtimes that need attention |
| Applications | Every application with its status; each one has Overview, Replicas, Routes, Cron jobs, Deployments, and Manifest tabs |
| Runtimes | Every managed replica and cron run, with live logs |
| Nodes | Every node with its state; each node has a detail page with actions |
| Jobs | One-off jobs, with cancel and retry |
| Events | Recent cluster events |

The Deployments tab of an application lists its history, compares revisions,
and offers rollback. The Cron jobs tab lists recent runs. Logs open in a drawer
that follows new output; pause and resume it with its button and close it with
`Esc`.

Press `⌘K` or `Ctrl+K`, or `/` outside a text field, to open the command
palette. It jumps to any page, application, node, or runtime log, and switches
between light and dark themes. The sidebar also offers a system, light, or dark
theme.

The dashboard refreshes every 2 seconds, and every 10 seconds while the tab is
hidden. It refreshes immediately when you return to the tab.

### Actions

The dashboard can cordon, uncordon, drain, and toggle ingress on nodes, roll an
application back to an earlier revision, and cancel or retry one-off jobs. Each
action opens a confirmation dialog that explains its effect first. Deploying,
running new jobs, and changing secrets stay in the CLI.

### Security

The dashboard is designed so the browser never holds cluster credentials:

- It listens on `127.0.0.1` only, so it is reachable from this machine alone.
  Requests whose `Host` is not a loopback address are rejected, which blocks
  DNS-rebinding attacks.
- The printed URL contains a random token that works once. Opening it sets an
  HttpOnly, `SameSite=Strict` session cookie and redirects to the dashboard. A
  URL recovered later from shell or browser history cannot open a new session;
  restart `orch web` for a new link.
- The administrator credentials, SSH tunnel, or socket connection stay inside
  the `orch` process. Every controller call uses the target's normal
  authenticated route.
- Changes require a same-origin request with a JSON body, and the page is
  served with a strict Content Security Policy and `X-Frame-Options: DENY`.

If the page says `open the URL printed by orch web`, the session cookie is
missing; use the link from the terminal. If it says the link was already used,
restart `orch web`.

## Next steps

- [CLI reference](cli.md) lists every command and flag.
- [Cluster operations](operations.md) covers day-to-day tasks.
- [Security and recovery](security.md) explains administrator access and
  certificate renewal.
