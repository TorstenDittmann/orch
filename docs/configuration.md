# Applications

Define an application in a typed Pkl file named `orch.pkl`.

```pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "hello"

services {
  ["web"] {
    image = "ghcr.io/acme/hello:1.2.3"
    replicas = 2
    resources {
      cpu = "250m"
      memory = "128Mi"
    }
    http = new Http {
      port = 8080
      domains { "hello.example.com" }
      healthPath = "/healthz"
    }
  }
}
```

Check a file before deploying it:

```console
orch validate
orch render
orch plan
```

## Services

Each service requires an OCI image. The most commonly used settings are:

| Setting | Default | Purpose |
| --- | --- | --- |
| `image` | required | OCI image tag or digest |
| `replicas` | `1` | Number of running instances |
| `command` | image default | Replacement entry point |
| `args` | image default | Command arguments |
| `restart` | `"always"` | `"always"`, `"on-failure"`, or `"never"` |
| `resources.cpu` | `"100m"` | CPU reservation or limit |
| `resources.memory` | `"128Mi"` | Memory limit |
| `env` | empty | Plain environment variables |
| `dependsOn` | empty | Services that must be available before startup |
| `constraints` | empty | Required node labels |

Use `http` for HTTP traffic and `tcp` for other TCP services. A service
cannot define both.

## Environment and secrets

Plain values can live in the application:

```pkl
env {
  ["APP_ENV"] = "production"
}
```

Store sensitive values through stdin:

```console
printf '%s' "$DATABASE_URL" | orch secret set hello database-url \
  --from-stdin --target production
```

Reference the stored name from the application:

```pkl
secretEnv {
  ["DATABASE_URL"] = "database-url"
}
```

Secrets can also be projected as files below `/run/secrets`:

```pkl
secretFiles {
  new SecretFile {
    source = "signing-key"
    target = "/run/secrets/signing-key"
  }
}
```

For private image registries:

```console
printf '%s' "$REGISTRY_TOKEN" | orch registry login registry.example:5443 \
  --username robot --password-stdin --target production
```

## Placement

Add labels to nodes, then constrain a service to matching nodes:

```pkl
constraints {
  ["region"] = "eu-west"
  ["tier"] = "compute"
}
```

Constraints are required matches. If no node has all requested labels, the
service remains blocked instead of running elsewhere. Replicas spread across
eligible nodes when capacity permits.

## Local volumes

Local volumes stay on one worker. A service that mounts one must have exactly
one replica.

```pkl
services {
  ["database"] {
    image = "postgres:17"
    mounts {
      new Mount {
        source = "database"
        target = "/var/lib/postgresql/data"
      }
    }
  }
}

volumes {
  ["database"] {}
}
```

If that worker is lost, orch reports `volume_node_lost`. It does not create an
empty replacement volume on another worker until you remove the lost worker.

Add `backup` to have the worker archive the volume on a schedule:

```pkl
volumes {
  ["database"] {
    backup = new VolumeBackup {}
  }
}
```

That keeps 7 daily backups, taken at 03:00 UTC. See
[volume backups](operations.md#volume-backups).

## Schedules

Schedules use five UTC cron fields:

```pkl
cronJobs {
  ["cleanup"] {
    image = "ghcr.io/acme/cleanup:1.0"
    schedule = "0 2 * * *"
    timeout = "10m"
  }
}
```

List recent runs and read their logs:

```console
orch cron runs hello cleanup
orch cron logs hello cleanup RUN_ID
```

Overlapping runs are skipped and missed runs are not replayed.

### Reading a local volume

A cron job can mount a service's local volume read-only, for example to copy
it somewhere else. Each run is placed on the worker that holds the volume:

```pkl
cronJobs {
  ["database-backup"] {
    image = "ghcr.io/acme/backup:1.0"
    schedule = "0 3 * * *"
    mounts {
      new Mount {
        source = "database"
        target = "/backup/source"
        readOnly = true
      }
    }
  }
}
```

`readOnly` must be `true`, and every mounted volume must belong to the same
service in the application. The job waits until that service has been placed.
The service keeps writing while the job reads, so use a copy method that is
safe for the data, such as the database's own snapshot or backup tool.

## Rollouts

```pkl
deploy {
  maxSurge = 1
  maxUnavailable = 0
  healthDeadline = "2m"
  autoRollback = true
}
```

`maxSurge` and `maxUnavailable` control how much extra capacity and
unavailability a rolling update may use. They cannot both be zero.
Applications whose services publish node-local ports must use `maxSurge = 0`
and `maxUnavailable = 1`; see
[node-local TCP and UDP](networking.md#node-local-tcp-and-udp).

See [deployments](runtime.md) for update and rollback behavior, or
[networking](networking.md) to expose a service.
