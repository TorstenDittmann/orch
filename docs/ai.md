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

Claude Code:

```console
claude mcp add orch -- orch mcp
```

Codex:

```console
codex mcp add orch -- orch mcp
```

Claude Desktop, Cursor, Windsurf and most other clients read a JSON file
(`claude_desktop_config.json`, `.cursor/mcp.json`, and so on):

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

VS Code uses `.vscode/mcp.json` with `"servers"` in place of `"mcpServers"`.
Desktop apps do not always see your shell's `PATH`; if the client cannot start
`orch`, use its full path from `command -v orch`.

Then ask in plain language, for example "why is the shop app not ready?",
"deploy orch.pkl and wait for it", or "roll checkout back to yesterday's
revision".

## Choosing the cluster and limiting access

| Command | What the assistant gets |
| --- | --- |
| `orch mcp` | Every tool, against the selected target |
| `orch mcp --target staging` | Every tool, always against `staging` |
| `orch mcp --read-only` | Only tools that read; nothing can be changed |

Remote tools also accept an optional `target` argument, so one server can work
with several clusters. Register a second server with `--read-only` if you want
an assistant to investigate production without being able to change it:

```console
claude mcp add orch-prod -- orch mcp --target production --read-only
```

Manifest paths are resolved from the directory the client starts `orch mcp`
in, which for Claude Code and Cursor is your project.

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

## Tools

Every tool runs the matching CLI command, and most return the same JSON as
`--json`. Tools that only read are marked read-only. Tools that delete or
replace running workloads or data are marked destructive, so clients ask
before running them.

| Area | Read-only | Changes the cluster |
| --- | --- | --- |
| Manifests | `schema`, `validate`, `plan`, `render` | |
| Cluster | `targets`, `status`, `list_nodes`, `events` | `update_node`*, `label_node`, `unlabel_node` |
| Applications | `list_apps`, `app_status`, `app_history`, `app_diff`, `wait_for_rollout`, `logs` | `deploy`, `rollback`, `restart`, `remove_app`*, `exec`* |
| Jobs and cron | `list_jobs`, `job_status`, `job_logs`, `cron_runs`, `cron_logs` | `run_job`*, `cancel_job`*, `retry_job`, `run_cron` |
| Secrets and registries | `list_secrets`, `list_registries` | `set_secret`*, `delete_secret`*, `registry_login`, `registry_logout`* |
| Volumes | `list_volumes`, `list_volume_backups` | `backup_volume`, `restore_volume`* |
| Databases | `list_databases` | `backup_database`, `restore_database`* |

\* marked destructive.

Secret values and registry passwords are sent to orch the same way
`--from-stdin` sends them, never on a command line, and orch never returns
them. They still pass through the assistant's conversation, so prefer setting
production secrets yourself with `orch secret set`.

What the server leaves out: interactive and long-running streams (`logs -f`,
`orch tui`, `orch web`), downloading volume backups, and host setup commands
such as `orch init` and `orch join`, which need root on a server.
