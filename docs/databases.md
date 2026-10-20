# Databases

Declare a database in `orch.pkl` and orch runs it for you. It generates the
credentials, gives your services a connection URL, and backs the data up when
you ask it to.

```pkl
amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "shop"

services {
  ["web"] {
    image = "ghcr.io/acme/shop:1.4.2"
    http = new Http { port = 8080; domains { "shop.example.com" } }
    dependsOn { "main"; "cache" }
    secretEnv {
      ["DATABASE_URL"] = "main-url"
      ["REDIS_URL"] = "cache-url"
    }
  }
}

databases {
  ["main"] = new Postgres {
    backup = new DatabaseBackup {}
  }
  ["cache"] = new Redis {}
}
```

`orch deploy` starts both databases next to `web`. `web` reads its connection
strings from the `main-url` and `cache-url` secrets; nobody has to create a
password.

## Engines

| Class | Image | Default version | Port | Backup |
| --- | --- | --- | --- | --- |
| `Postgres` | `postgres` | `17` | 5432 | `pg_dump` |
| `MySQL` | `mysql` | `8.4` | 3306 | `mysqldump` |
| `MariaDB` | `mariadb` | `11.4` | 3306 | `mariadb-dump` |
| `MongoDB` | `mongo` | `8.0` | 27017 | `mongodump` |
| `Redis` | `redis` | `7.4` | 6379 | Snapshot after `SAVE`, with append-only persistence on |
| `Valkey` | `valkey/valkey` | `8.1` | 6379 | Snapshot after `SAVE`, with append-only persistence on |
| `Dragonfly` | `docker.dragonflydb.io/dragonflydb/dragonfly` | `latest` | 6379 | Snapshot after `SAVE` |
| `Garnet` | `ghcr.io/microsoft/garnet` | `latest` | 6379 | None: data is kept in memory only |
| `ClickHouse` | `clickhouse/clickhouse-server` | `25.8` | 9000 (native), 8123 (HTTP) | `BACKUP DATABASE` |
| `LibSQL` | `ghcr.io/tursodatabase/libsql-server` | `latest` | 8080 | SQLite online backup of the data files |

Valkey, Dragonfly and Garnet speak the Redis protocol, so Redis clients work
with them unchanged. Garnet's image runs as a user that cannot write to a local
volume, so orch runs it without one: it starts empty after every restart and
suits caches only.

Every engine accepts the same fields:

| Field | Type | Default | Rules |
| --- | --- | --- | --- |
| `version` | String | per engine, above | Tag of the official image |
| `image` | String? | `null` | Full image reference instead, for a mirror or a compatible build; `version` is then only shown, not used |
| `resources` | `Resources` | `100m` CPU; 256Mi for Postgres and MariaDB, 512Mi for MySQL and MongoDB, 128Mi otherwise | See [Resources](manifest.md#resources) |
| `constraints` | `Mapping<String, String>` | empty | Node labels the database's node must have |
| `backup` | `DatabaseBackup?` | `null`, no backups | Set it to back the database up on a schedule |

`DatabaseBackup` has `schedule` (five UTC cron fields, default `"0 3 * * *"`)
and `keep` (1 to 365, default 7).

Database names are DNS labels of at most 54 characters, so the names derived
from them below stay valid.

## What orch creates

A database named `main` in application `shop` becomes:

- a service `main`, reachable at `main.shop.svc.internal` on the engine's port,
  with one replica and a one minute stop grace period;
- a local volume `main-data` with its files (except for Garnet);
- for the dump engines (Postgres, MySQL, MariaDB, MongoDB, ClickHouse) with
  backups on, a local volume `main-backup` that holds the latest dump;
- the secrets below, created on the first deploy that declares the database.

The service and volumes are ordinary parts of the application. They show up in
`orch app status`, `orch app diff`, `orch logs shop main` and `orch volume
list`, and a service or volume of the same name is rejected. Changing a
database's version or resources rolls it out like any other service: the old
replica stops before the new one starts on the same node and volume.

## Credentials

| Engine | Secrets | URL format |
| --- | --- | --- |
| Postgres | `main-password`, `main-url` | `postgres://orch:PASSWORD@main.shop.svc.internal:5432/main` |
| MySQL, MariaDB | `main-password`, `main-url` | `mysql://orch:PASSWORD@main.shop.svc.internal:3306/main` |
| MongoDB | `main-password`, `main-url` | `mongodb://orch:PASSWORD@main.shop.svc.internal:27017/main?authSource=admin` |
| Redis, Valkey, Dragonfly, Garnet | `main-password`, `main-url` | `redis://:PASSWORD@main.shop.svc.internal:6379` |
| ClickHouse | `main-password`, `main-url` | `clickhouse://orch:PASSWORD@main.shop.svc.internal:9000/main` |
| LibSQL | `main-jwt-key`, `main-token`, `main-url` | `http://main.shop.svc.internal:8080?authToken=TOKEN` |

The user is `orch` and the database is named after the entry; ClickHouse turns
dashes in that name into underscores. MySQL and
MariaDB use the same password for `root`. LibSQL clients authenticate with
`main-token`, a full-access token signed for the public key in `main-jwt-key`;
pass the token separately if your client does not read `authToken` from the URL.

orch only creates a secret that does not exist yet and never changes one, so a
database keeps the password it was initialized with. To choose your own
password, set it with `orch secret set shop main-password --from-stdin` before
the first deploy. The engines only read the password when they initialize an
empty volume, so changing it later means changing it inside the database too.

## Backups

Backups are off until you add a `backup`:

```pkl
["main"] = new Postgres {
  backup = new DatabaseBackup {
    schedule = "0 */6 * * *"
    keep = 14
  }
}
```

They run on the database's node.

- **Dump engines** write a fresh dump into the `main-backup` volume, then orch
  archives that volume. A dump is consistent even while the database takes
  writes.
- **Redis, Valkey and Dragonfly** run `SAVE`, then orch archives the
  `main-data` volume.
- **LibSQL** archives `main-data`; SQLite files are copied with SQLite's online
  backup.

A backup that fails is retried an hour later. See and take backups with:

```sh
orch db list
orch db backups shop main
orch db backup shop main
```

`orch volume download shop main-backup BACKUP` downloads an archive, which for
a dump engine contains the dump (`dump.pgc`, `dump.sql`, `dump.archive.gz`, or
ClickHouse's `dump` directory).

## Restore

```sh
orch db restore shop main            # the newest backup
orch db restore shop main 20261020T030000Z
```

For a dump engine, orch swaps the backup into `main-backup`, waits until the
database accepts connections, and loads the dump into it: `pg_restore --clean`,
the SQL dump (which drops and recreates the database), `mongorestore --drop`,
or ClickHouse's `RESTORE` after dropping the database. For the Redis-compatible
engines and LibSQL it swaps the backup into `main-data` and restarts
the database on it; the data it replaced is kept as a `-before-restore`
backup.

Loading a dump is limited to five minutes. For a larger database, download the
dump and load it yourself with `orch exec`.

## Limits

- A database runs one replica on one node, like any service with a local
  volume. There is no replication or failover.
- Backups stay on the database's node. Copy them elsewhere with `orch volume
  download` if you need them off the node.
- Removing a database from `orch.pkl` stops it. Its secrets stay until you
  delete them with `orch secret delete`.
