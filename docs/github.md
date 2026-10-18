# Deploy from GitHub

Every push to your main branch can build a new image and roll it out with a
GitHub Actions workflow. The workflow uses an [SSH target](targets.md#ssh-targets),
the same way you deploy from your workstation.

## Set up access once

Create a key pair that is only used for deployments, and allow it on the
controller:

```console
ssh-keygen -t ed25519 -N '' -C github-deploy -f orch-deploy
ssh-copy-id -i orch-deploy.pub root@203.0.113.10
ssh-keyscan 203.0.113.10 > orch-known-hosts
```

In the repository's **Settings > Secrets and variables > Actions**, add:

| Secret | Value |
| --- | --- |
| `ORCH_SSH_KEY` | Contents of `orch-deploy` |
| `ORCH_KNOWN_HOSTS` | Contents of `orch-known-hosts` |

Then delete the local copies of the private key. Anyone who can run workflows
in this repository can administer the cluster through this key, so remove it
from the controller's `authorized_keys` to revoke access.

If the image is private, give the cluster read access to it once:

```console
printf '%s' "$GHCR_READ_TOKEN" | orch registry login ghcr.io --username USER --password-stdin
```

## The workflow

Save this as `.github/workflows/deploy.yml`, replacing `ghcr.io/acme/hello`
with your image (registry paths must be lowercase) and `203.0.113.10` with your
controller:

```yaml
name: deploy
on:
  push:
    branches: [main]
permissions:
  contents: read
  packages: write
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
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
      - name: Deploy
        env:
          ORCH_SSH_KEY: ${{ secrets.ORCH_SSH_KEY }}
          ORCH_KNOWN_HOSTS: ${{ secrets.ORCH_KNOWN_HOSTS }}
        run: |
          mkdir -p ~/.ssh
          printf '%s\n' "$ORCH_KNOWN_HOSTS" > ~/.ssh/known_hosts
          eval "$(ssh-agent -s)"
          printf '%s\n' "$ORCH_SSH_KEY" | ssh-add -
          curl -fsSL https://raw.githubusercontent.com/TorstenDittmann/orch/main/install.sh | sh
          orch target add production ssh://root@203.0.113.10
          orch deploy --wait --image "ghcr.io/acme/hello@${{ steps.build.outputs.digest }}"
```

`--image` with a bare reference replaces the image of every service and cron
job in `orch.pkl` that uses the same repository, so the manifest keeps a
readable tag such as `ghcr.io/acme/hello:latest` and never needs editing in CI.
To replace a single workload, name it: `--image web=ghcr.io/acme/hello@sha256:...`.
See [`orch deploy`](cli.md#orch-deploy).

With `--wait` the job stays running until the rollout is ready and fails if it
fails, times out, or rolls back, so a red commit means the deployment did not
go out. A GitHub environment with required reviewers turns the job into an
approval gate.

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
