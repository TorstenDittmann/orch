# Deploy from GitHub

Every push to your main branch can build a new image and roll it out with a
GitHub Actions workflow. No secrets are stored in GitHub: the workflow proves
which repository and branch it runs for with GitHub's OIDC token, and the
controller gives it a 15-minute credential that can only deploy one
application.

## Set it up

Run this once from your workstation, inside the repository's checkout:

```console
$ orch github init
Trusted acme/hello to deploy hello from branch main.
Wrote .github/workflows/deploy.yml: every push to main builds ghcr.io/acme/hello and deploys it.

Commit and push it to deploy. No secrets are needed.
```

`init` reads the repository from the `origin` remote, the application name from
`orch.pkl`, and the branch from the remote's default branch. It tells the
controller to trust that repository and writes a workflow like this one:

```yaml
name: deploy
on:
  push:
    branches: [main]
  workflow_dispatch:
permissions:
  contents: read
  packages: write
  id-token: write
concurrency:
  group: deploy-hello
  cancel-in-progress: false
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: TorstenDittmann/orch@v0.2.0
        with:
          cluster: https://203.0.113.10:9443#sha256:3f9a…
          app: hello
          image: ghcr.io/acme/hello
```

`cluster` is the controller address and the fingerprint of its certificate
authority. It is not a secret; the fingerprint lets the workflow refuse
anything that only pretends to be your controller. `id-token: write` lets the
job request its OIDC token.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--app` | the manifest's `name` | The one application the repository may deploy |
| `--branch` | the remote's default branch | Only runs for this branch may deploy |
| `--environment` | none | Only jobs that use this GitHub environment may deploy, so its required reviewers become an approval gate |
| `--image` | see below | Image repository to build and push before deploying |
| `-f` | `orch.pkl` | Manifest to deploy, for a repository with several |
| `--repo` | the `origin` remote | `OWNER/REPO` when the remote is not GitHub |
| `--force` | off | Replace an existing `deploy.yml` |

When a `Dockerfile` sits next to the manifest and the manifest's workloads all
use one image repository, the workflow builds that image from the manifest's
directory. Without one, it deploys the manifest's images as written.

The workflow builds for the architectures of the cluster's nodes. An all-arm64
cluster builds on GitHub's `ubuntu-24.04-arm` runner, and a cluster that mixes
amd64 and arm64 nodes builds one image for both with `platforms`. Rerun
`orch github init --force` after adding nodes of another architecture.

`orch github list` shows every trusted repository. `orch github untrust
acme/hello --app hello` stops new runs at once; a credential a run already
received expires within 15 minutes. `orch github trust acme/hello --app hello`
trusts a repository without a local checkout and prints the workflow instead
of writing it.

The first deploy binds the trust to GitHub's IDs for the repository and its
owner. If the repository or its owner is later renamed or deleted and someone
else takes the name, their runs are refused. Run `orch github trust` again
when you recreate or transfer the repository yourself.

A repository may be trusted for several applications, for example in a
monorepo. Run `orch github init -f apps/api/orch.pkl` for each and rename the
written workflows so they do not replace each other.

## The deploy action

`TorstenDittmann/orch` is a GitHub Action. Each run it:

1. Installs the orch CLI matching the action's version, and the Pkl version
   that CLI needs.
2. When `image` is set, builds the image, tags it with the commit, and pushes
   it. It signs in to `ghcr.io` with the job's own token; push to another
   registry by adding a login step before it.
3. Runs `orch deploy --wait`, replacing the manifest's image with the build
   that was just pushed, by digest.

| Input | Default | Purpose |
| --- | --- | --- |
| `cluster` | required | Cluster address from `orch github init` |
| `app` | none | Application to deploy, when the repository is trusted for several |
| `image` | none | Image repository to build and push |
| `context` | `.` | Docker build context |
| `dockerfile` | `Dockerfile` in the context | Dockerfile path |
| `platforms` | the runner's own | Image platforms, such as `linux/amd64,linux/arm64` |
| `file` | `orch.pkl` | Manifest to deploy |
| `version` | the action's tag, or the latest release | orch CLI version to install |

The pushed image is available to later steps as `steps.<id>.outputs.image`.

The action deploys with `orch deploy --image`, which replaces the image of
every service and cron job in `orch.pkl` that uses the same repository, so the
manifest keeps a readable tag such as `ghcr.io/acme/hello:latest` and never
needs editing in CI. To replace a single workload in your own step, name it:
`--image web=ghcr.io/acme/hello@sha256:...`. See
[`orch deploy`](cli.md#orch-deploy).

With `--wait` the job stays running until the rollout is ready and fails if it
fails, times out, or rolls back, so a red commit means the deployment did not
go out.

If the image is private, give the cluster read access to it once:

```console
printf '%s' "$GHCR_READ_TOKEN" | orch registry login ghcr.io --username USER --password-stdin
```

## What the workflow can do

The credential can deploy, roll back, and read the status and history of its
one application. It cannot read secrets, open a shell in containers, change
nodes, registries or other applications, or trust more repositories. Deploying
does run code you choose with that application's secrets, so protect the
branch the way you would protect production.

Only runs started by `push`, `workflow_dispatch`, `schedule` or
`repository_dispatch` can deploy. The controller refuses
`pull_request_target`, `workflow_run` and other events that run on the
trusted branch but may build code from a pull request.

The controller needs outbound HTTPS to `token.actions.githubusercontent.com`
to fetch GitHub's signing keys, and GitHub-hosted runners must reach the
controller port. A cluster that runners cannot reach can deploy from a
self-hosted runner, with an [SSH target](#deploy-with-an-ssh-key) from
inside its network, or by pulling new images itself with
[`autoUpdate`](runtime.md#automatic-image-updates) while the workflow only
pushes the image.

## What history shows

Deployments started from GitHub Actions record the repository, commit, branch,
actor and a link to the run. They appear in `orch app history`, in the TUI's
deployment details, and in the dashboard:

```console
$ orch app history hello
7  rev_9c1…  2026-10-18T21:30:12Z  SUCCEEDED current active
  from acme/hello@3f9a2c1d4e5b (main) by octocat https://github.com/acme/hello/actions/runs/42
  service web  ghcr.io/acme/hello@sha256:…
```

Rolling back with `orch rollback` works the same as for any other deployment.

## Deploy with an SSH key

Without a reachable controller port, a workflow can also use an
[SSH target](targets.md#ssh-targets). Store a deploy key that is allowed in the
controller's `authorized_keys` as `ORCH_SSH_KEY` and the output of
`ssh-keyscan HOST` as `ORCH_KNOWN_HOSTS`, then replace the last step with:

```yaml
      - name: Deploy
        env:
          ORCH_SSH_KEY: ${{ secrets.ORCH_SSH_KEY }}
          ORCH_KNOWN_HOSTS: ${{ secrets.ORCH_KNOWN_HOSTS }}
        run: |
          mkdir -p ~/.ssh
          printf '%s\n' "$ORCH_KNOWN_HOSTS" > ~/.ssh/known_hosts
          eval "$(ssh-agent -s)"
          printf '%s\n' "$ORCH_SSH_KEY" | ssh-add -
          orch target add production ssh://root@203.0.113.10
          orch deploy --wait --image "ghcr.io/acme/hello@${{ steps.build.outputs.digest }}"
```

That key gives full administrator access, so prefer `orch github trust`
whenever runners can reach the controller.
