# AI assistants

orch has a built-in [Model Context Protocol](https://modelcontextprotocol.io)
server, so an AI assistant such as Claude Code, Claude Desktop, Cursor or Codex
can operate your cluster: check status, read logs, write and deploy manifests,
roll back, manage secrets, run jobs and back up volumes.

There is nothing extra to install. `orch mcp` runs the server over stdio with
the same target and credentials as the CLI, so the assistant can do exactly what
you can do from your terminal, and nothing more.

## Add orch to your assistant

Add a target first if you have not (`orch target add NAME ssh://root@CONTROLLER`),
then register `orch mcp` with your client.

<!-- tabs -->

**Claude Code**

```console
claude mcp add orch -- orch mcp
```

**Codex**

```console
codex mcp add orch -- orch mcp
```

**Cursor**

Add to `.cursor/mcp.json` in your project, or `~/.cursor/mcp.json` for every
project:

```json
{
  "mcpServers": {
    "orch": {
      "command": "orch",
      "args": ["mcp"]
    }
  }
}
```

**opencode**

Add to `opencode.json` in your project, or `~/.config/opencode/opencode.json`
for every project:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "orch": {
      "type": "local",
      "command": ["orch", "mcp"]
    }
  }
}
```

<!-- /tabs -->

Claude Desktop, Windsurf and most other clients read the same `mcpServers`
JSON as Cursor from their own file, such as `claude_desktop_config.json`.
VS Code uses `.vscode/mcp.json` with `"servers"` in place of `"mcpServers"`.
Desktop apps do not always see your shell's `PATH`; if the client cannot start
`orch`, use its full path from `command -v orch`.

Then ask in plain language, for example "why is the shop app not ready?",
"deploy orch.pkl and wait for it", or "roll checkout back to yesterday's
revision".

## Interactive views

In clients that support [MCP Apps](https://github.com/modelcontextprotocol/ext-apps),
such as Claude, some tools answer with an interactive view next to the text
the assistant reads:

| Tool | View |
| --- | --- |
| `status` | The cluster's server, network, certificate expiry and backups |
| `list_apps` | Every application and its rollout status; select one to open it |
| `app_status` | Replicas with phase, health and restarts, routes and cron jobs |
| `deploy`, `rollback`, `wait_for_rollout` | The rollout, followed live until it is ready or fails |

Views refresh through orch's own read-only tools, with the same target and
credentials, and need no setup. Clients without MCP Apps get the same text
answers as before.

## Choosing the cluster and limiting access

| Command | What the assistant gets |
| --- | --- |
| `orch mcp` | Tools that read, deploy, roll back, back up, log in to registries and label nodes, against the selected target |
| `orch mcp --target staging` | The same tools, always against `staging` |
| `orch mcp --allow-destructive` | Also the destructive tools, such as `exec`, `remove_app` and `set_secret` |
| `orch mcp --read-only` | Only tools that read; nothing can be changed |

Remote tools also accept an optional `target` argument, so one server can work
with several clusters. Register a second server with `--read-only` if you want
an assistant to investigate production without being able to change it:

```console
claude mcp add orch-prod -- orch mcp --target production --read-only
```

Manifest paths are resolved from the directory the client starts `orch mcp`
in, which for Claude Code and Cursor is your project.

Destructive tools are off unless you pass `--allow-destructive`. Logs and job
output are written by your applications, and the assistant reads them; text
planted there should not be able to talk it into deleting an application or
running a command in a replica.

`--read-only` hides the tools that change things, but the assistant still runs
with your credentials. For an access limit the controller enforces, give it an
access token instead.

## Agents on other machines

An agent in a cloud sandbox, on a server, or in CI should not get your
administrator credentials. Give it an access token for the one application it
works on:

```console
$ orch token create --app shop --read-only
Created token shop-read-only. It can inspect shop until 2026-11-18.
orch_aHR0cHM6Ly8yMDMuMC4xMTMuMTA6OTQ0MyNzaGEyNTY6…
```

Set the printed value as `ORCH_TOKEN` where the agent runs. `orch mcp` and
every other orch command use it automatically, and need no target:

```console
claude mcp add orch -e ORCH_TOKEN=orch_… -- orch mcp
```

| Token | What the agent can do |
| --- | --- |
| `orch token create --app shop --read-only` | Read `shop`'s status, logs, cron runs, deployment history, and diffs, and wait for rollouts |
| `orch token create --app shop` | Also deploy and roll back `shop` |

Nothing else on the cluster is reachable with a token: other applications,
nodes, secrets, volumes, jobs, and `exec` all answer "permission denied". Tokens
expire after 30 days unless you pass `--ttl`. `orch token list` shows when each
was last used, and `orch token revoke NAME` stops one within 15 minutes.

In GitHub Actions you need no token at all: `orch github trust` lets the
workflow sign in with GitHub's own identity. See [deploy from GitHub](github.md).

## Remote MCP over a URL

Any MCP client that connects to remote servers by URL with OAuth, such as
Claude connectors, ChatGPT, Cursor, VS Code or a hosted agent, cannot start
`orch mcp` on your computer. The controller can serve the same tools at a URL
instead, as a standard OAuth-protected MCP server:

```console
$ orch mcp expose mcp.example.com
Remote MCP is served at https://mcp.example.com/mcp
```

Point DNS for the domain at the controller's server. The ingress on that
server gets an HTTPS certificate for it, like for an application, and
forwards requests to the controller on the host's loopback interface.

Then add `https://mcp.example.com/mcp` to your client as a remote MCP server.
It discovers the authorization server, registers itself and opens an approval
page that shows its name, where it will return to, and a code. Approve it from
any machine with orch administrator access:

```console
$ orch mcp approve K7XQ-2MNP
Approved Claude (returning to claude.ai) as mcp-claude-3f2a. It can read every application until 2026-12-13.
```

The page continues by itself and the assistant is connected. There is no
password or token to copy.

| Command | What the assistant can do |
| --- | --- |
| `orch mcp approve CODE` | Read the cluster: status, logs, history, events, jobs, volumes |
| `orch mcp approve CODE --deploy` | Also roll back, restart, back up, retry jobs and run cron jobs |
| `orch mcp approve CODE --allow-destructive` | Everything the MCP tools can do, including removing apps, changing secrets and running commands in replicas |
| `... --app shop` | Any of the above, for `shop` only |

The controller enforces the level on every call, not just the tool list:
each one maps to the API calls it allows, and nothing else, such as creating
tokens, approving other connections or removing nodes, is reachable at any
level. Connections show up in `orch token list` with when they were last
used, expire after 30 days unless you pass `--ttl`, and `orch token revoke
NAME` ends one at once. `orch mcp expose --off` stops serving the URL.

Deploying a manifest stays with the CLI, `orch mcp` on your computer and
GitHub Actions: manifests are evaluated where their files are, and the
controller never runs Pkl. Over the URL, an assistant works with what is
already deployed.

Under the hood the controller is a small OAuth 2.1 server: assistants register
themselves, use the authorization code flow with PKCE, and get access tokens
that last an hour and refresh tokens that work once. An `ORCH_TOKEN` access
token also works as a bearer token, with the same limits as on the CLI.

## Tools

Every tool runs the matching CLI command, and most return the same JSON as
`--json`. Tools that only read are marked read-only. Tools that delete or
replace running workloads or data are marked destructive, so clients ask
before running them, and they are only offered with `--allow-destructive`.

| Area | Read-only | Changes the cluster |
| --- | --- | --- |
| Manifests | `schema`, `validate`, `plan`, `render` | |
| Cluster | `targets`, `status`, `list_nodes`, `events` | `update_node`*, `label_node`, `unlabel_node` |
| Applications | `list_apps`, `app_status`, `app_history`, `app_diff`, `wait_for_rollout`, `logs` | `deploy`, `rollback`, `restart`, `remove_app`*, `exec`* |
| Jobs and cron | `list_jobs`, `job_status`, `job_logs`, `cron_runs`, `cron_logs` | `run_job`*, `cancel_job`*, `retry_job`, `run_cron` |
| Secrets and registries | `list_secrets`, `list_registries` | `set_secret`*, `delete_secret`*, `registry_login`, `registry_logout`* |
| Volumes | `list_volumes`, `list_volume_backups` | `backup_volume`, `restore_volume`*, `move_volume`* |
| Databases | `list_databases` | `backup_database`, `restore_database`* |

\* destructive, offered only with `--allow-destructive`.

Secret values and registry passwords are sent to orch the same way
`--from-stdin` sends them, never on a command line, and orch never returns
them. They still pass through the assistant's conversation, so prefer setting
production secrets yourself with `orch secret set`.

What the server leaves out: interactive and long-running streams (`logs -f`,
`orch tui`, `orch web`), downloading volume backups, and host setup commands
such as `orch init` and `orch join`, which need root on a server.
