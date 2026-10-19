# Deploy from GitHub

Every push to your main branch can build a new image and roll it out with a
GitHub Actions workflow. No secrets are stored in GitHub: the workflow proves
which repository and branch it runs for with GitHub's OIDC token, and the
controller gives it a 15-minute credential that can only deploy one
application.

## Trust the repository once

From your workstation:

```console
$ orch github trust acme/hello --app hello
Trusted acme/hello to deploy hello from branch main.

Deploy on every push with .github/workflows/deploy.yml; no secrets are needed:
...
          ORCH_CLUSTER: https://203.0.113.10:9443#sha256:3f9a…
```

The command prints a workflow you can paste. `ORCH_CLUSTER` is the controller
address and the fingerprint of its certificate authority. It is not a secret;
the fingerprint lets the workflow refuse anything that only pretends to be
your controller.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--app` | required | The one application the repository may deploy |
| `--branch` | `main` | Only runs for pushes to this branch may deploy |
| `--environment` | none | Only jobs that use this GitHub environment may deploy, so its required reviewers become an approval gate |

`orch github list` shows every trusted repository. `orch github untrust
acme/hello --app hello` stops new runs at once; a credential a run already
received expires within 15 minutes.

A repository may be trusted for several applications, for example in a
monorepo. Its workflow then sets `ORCH_APP` to the application it deploys.

## The workflow

This workflow builds the image, pushes it to the GitHub Container Registry, and
deploys exactly that build. Replace `ghcr.io/acme/hello` with your image
(registry paths must be lowercase) and `ORCH_CLUSTER` with the value
`orch github trust` printed:

```yaml
name: deploy
on:
  push:
    branches: [main]
permissions:
  contents: read
  packages: write
  id-token: write
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - id: build
        uses: docker/build-push-action@v6
        with:
          push: true
          tags: ghcr.io/acme/hello:${{ github.sha }}
      - run: curl -fsSL https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.sh | sh
      - run: orch deploy --wait --image "ghcr.io/acme/hello@${{ steps.build.outputs.digest }}"
        env:
          ORCH_CLUSTER: https://203.0.113.10:9443#sha256:3f9a…
```

`id-token: write` lets the job request its OIDC token; without it the deploy
step stops with an error saying so.

`--image` with a bare reference replaces the image of every service and cron
job in `orch.pkl` that uses the same repository, so the manifest keeps a
readable tag such as `ghcr.io/acme/hello:latest` and never needs editing in CI.
To replace a single workload, name it: `--image web=ghcr.io/acme/hello@sha256:...`.
See [`orch deploy`](cli.md#orch-deploy).

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
self-hosted runner, or with an [SSH target](#deploy-with-an-ssh-key) from
inside its network.

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
