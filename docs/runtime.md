# Deployments

Use deployments to apply and track changes to an application.

## Deploy

Validate and inspect the proposed change:

```console
orch validate
orch plan
```

Deploy and wait for the rollout:

```console
orch deploy --wait --target production
```

Submitting the same resolved configuration again is a no-op. Submitting an
updated image or configuration creates a new revision.

## Status

```console
orch app status hello --target production
orch app history hello --target production
```

Typical states are:

| State | Meaning |
| --- | --- |
| `PENDING` | Accepted and waiting to start |
| `DEPLOYING` | New instances are being created or checked |
| `HEALTHY` | Requested instances and routes are ready |
| `BLOCKED` | Placement or a required resource is unavailable |
| `FAILED` | The rollout could not complete |

Run `orch tui --target production` for a live view of applications, workers,
deployments, events, and logs.

## Health checks

An HTTP service with `healthPath` receives an HTTP GET every two seconds.
Traffic is sent only to ready instances. Without `healthPath`, an HTTP or TCP
endpoint uses a TCP readiness check.

```pkl
http = new Http {
  port = 8080
  healthPath = "/healthz"
}
```

Keep the health endpoint fast and independent of optional downstream services.

## Updates

Change the application file, inspect the plan, and deploy it again:

```console
orch plan
orch deploy --wait --target production
```

The `deploy` block controls rollout capacity and deadline. With
`autoRollback = true`, a failed update restores the last healthy revision.
Existing instances stay available within the configured availability budget
while replacements become ready.

## Automatic image updates

With `autoUpdate = true`, the controller checks the registry every five
minutes for the image tags in the current deployment, such as
`ghcr.io/acme/hello:main`. When a tag points to a new digest, it rolls out a
new revision the same way `orch deploy` would, with the same health checks and
automatic rollback:

```pkl
services {
  ["web"] {
    image = "ghcr.io/acme/hello:main"
  }
}

deploy {
  autoUpdate = true
}
```

CI then only pushes the image, and nothing has to reach the controller. The
cluster needs outbound access to the registry, plus
[registry credentials](cli.md#orch-registry) for a private image.

- Images pinned by digest are never changed.
- A revision that failed is not retried; the next update waits for the tag to
  move again.
- `orch rollback` pauses updates, so they never undo a rollback. The next
  `orch deploy` turns them back on.
- History shows these deployments as `by auto-update`.

## Rollbacks

List deployment history and compare revisions before restoring one:

```console
orch app history hello --target production
orch app diff hello REVISION --target production
orch rollback hello REVISION --wait --target production
```

A rollback creates a new deployment from the selected stored revision and
reuses its resolved image digests.

## Placement

orch considers CPU, memory, node labels, and local-volume ownership when placing
instances. A service remains `BLOCKED` when no eligible worker can run it.

When a worker disappears, services without local volumes can be placed on other
eligible workers. A service with a local volume stays bound to the worker that
owns its data and reports `volume_node_lost` until that worker returns. Removing
the lost worker releases the claim, and the service then starts elsewhere with
an empty volume; restore the data separately.

Agents keep their assigned workloads running while the controller is
unavailable. New deployments, scheduling decisions, and cluster changes wait
for the controller to return.

## Logs and exec

Use the TUI for discovery, or the application commands for scripts:

```console
orch app status hello --target production
orch logs hello web --replica 1 --tail 100 --target production
orch exec hello web --replica 1 --target production -- cat /etc/hostname
```

`orch exec` runs one non-interactive command in a replica and prints its output.

See [cluster operations](operations.md) for node maintenance and application
removal.
