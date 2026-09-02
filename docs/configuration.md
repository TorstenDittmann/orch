# v1alpha1 application configuration

orch.toml is decoded strictly. Misspelled or unknown fields fail validation.
The schema version is mandatory, names use lowercase DNS labels, and at least
one service is required.

The machine-readable schema is [schema/orch.schema.json](../schema/orch.schema.json).
Run orch schema to print the exact schema shipped by the binary.
Run orch render to inspect the canonical manifest with all defaults resolved.
The controller repeats the same strict parse, default, validation, and
canonicalization steps for every `orch deploy`; local validation is not a trust
boundary.

## Top level

| Field | Required | Meaning |
| --- | --- | --- |
| schema | yes | Must be v1alpha1. |
| name | yes | Application DNS label, at most 63 characters. |
| services | yes | Map of service name to service definition. |
| volumes | no | Named local volumes. |
| deploy | no | Application rolling-update defaults. |

## Service

| Field | Default | Meaning |
| --- | --- | --- |
| image | required | OCI image reference; whitespace is rejected. |
| command | image default | Replacement entry point. |
| args | image default | Arguments passed to the command. |
| replicas | 1 | Desired instances, from 1 through 100. |
| restart | always | always, on-failure, or never. |
| resources.cpu | 100m | Whole CPUs or positive millicpu, such as 500m. |
| resources.memory | 128Mi | Positive Ki, Mi, Gi, or Ti value. |
| http | absent | Internal HTTP endpoint and optional public domains. |
| env | empty | Plain environment variables. |
| secret_env | empty | Environment variable to secret-name references. |
| secret_files | empty | Secret-name to in-memory file projections. |
| constraints | empty | Required node label/value pairs. |
| mounts | empty | Named-volume mounts. |

An HTTP block requires port. Configuring any domains also requires health_path.
HTTPS defaults to true. Wildcards are not supported by v1alpha1.

~~~toml
[services.web.http]
port = 8080
domains = ["www.example.com"]
health_path = "/healthz"
https = true
~~~

Plain and secret environment maps may not define the same variable. Secret
values are set through the future controller API; the manifest contains names
only.

~~~toml
[services.web.secret_env]
DATABASE_URL = "database-url"

[[services.web.secret_files]]
source = "signing-key"
target = "/run/secrets/signing-key"
~~~

Secret file targets must be clean paths below /run/secrets.

## Local volumes

v1alpha1 supports only the local driver. A local volume may belong to one
service, and that service must have exactly one replica. Mount targets must be
absolute and may not replace protected paths such as /proc, /sys, /dev, or
/run/orch.

~~~toml
[[services.database.mounts]]
source = "database"
target = "/var/lib/postgresql/data"

[volumes.database]
driver = "local"
~~~

These rules make the future scheduler's failure behavior explicit: the
allocation stays attached to the node holding the data.

## Deployment defaults

~~~toml
[deploy]
max_surge = 1
max_unavailable = 0
health_deadline = "2m"
auto_rollback = true
~~~

The availability values cannot both be zero. The health deadline must be from
10 seconds through one hour.

## Canonical form and digests

Parsing applies defaults, normalizes whole CPU values to millicpu, sorts domain
and mount sets, and encodes a deterministic JSON representation. The displayed
sha256 digest identifies that resolved contract, not the TOML byte layout.
Equivalent field ordering therefore produces the same digest.

`orch plan` compares a target file with an optional `--against` file and remains
local/read-only. `orch deploy` sends the TOML to the authenticated controller,
which stores the canonical form as an immutable desired-state revision. A
formatting-only resubmission has the same digest. Until the agent/runtime slice
lands, every accepted revision has execution status `PENDING` and starts no
container.
