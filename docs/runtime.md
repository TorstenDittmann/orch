# Deployments

orch turns an application file into an immutable deployment revision. It
resolves image tags to digests, places replicas on workers with enough
capacity, waits for health checks, and switches ingress only to ready
instances.

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
owns its data and reports `volume_node_lost` until that worker returns or you
restore the data separately.

Agents keep their assigned workloads running while the controller is
unavailable. New deployments, scheduling decisions, and cluster changes wait
for the controller to return.

## Logs and exec

Use the TUI for discovery, or the application commands for scripts:

```console
orch app status hello --target production
orch logs hello web --replica 1 --tail 100 --target production
orch exec hello web --replica 1 --target production -- /bin/sh
```

See [cluster operations](operations.md) for node maintenance and application
removal.
