package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Database engines a manifest can declare under `databases`.
const (
	EnginePostgres   = "postgres"
	EngineMySQL      = "mysql"
	EngineMariaDB    = "mariadb"
	EngineMongoDB    = "mongodb"
	EngineRedis      = "redis"
	EngineLibSQL     = "libsql"
	EngineValkey     = "valkey"
	EngineDragonfly  = "dragonfly"
	EngineGarnet     = "garnet"
	EngineClickHouse = "clickhouse"
)

const (
	maxDatabases        = 20
	maxDatabaseName     = 54
	databaseUser        = "orch"
	databaseDumpMount   = "/backup"
	databasePasswordLen = 32
)

// Database marks a service that orch runs for a `databases` entry. The
// service, its volumes and its secret references are generated from it.
type Database struct {
	Engine  string `json:"engine"`
	Version string `json:"version"`
}

type databaseEngine struct {
	image string
	port  int
	// dataPath is empty for engines that keep their data in memory only.
	dataPath string
	env      func(name string) map[string]string
	// secretEnv maps environment variables to secret name suffixes.
	secretEnv map[string]string
	command   []string
	args      []string
	// dump engines keep a second volume of logical dumps; the others back
	// up their data volume directly.
	dumpFile string
	// prepareDump runs before each dump, for example to let the database
	// write to the dump volume.
	prepareDump string
	dumpCommand func(name, file string) string
	loadCommand func(name, file string) string
	readyScript func(name string) string
	before      []string
	url         func(host, name string, secrets map[string]string) string
}

var databaseEngines = map[string]databaseEngine{
	EnginePostgres: {
		image: "postgres", port: 5432, dataPath: "/var/lib/orch-postgres",
		env: func(name string) map[string]string {
			return map[string]string{"POSTGRES_USER": databaseUser, "POSTGRES_DB": name, "PGDATA": "/var/lib/orch-postgres"}
		},
		secretEnv: map[string]string{"POSTGRES_PASSWORD": "password"},
		dumpFile:  "dump.pgc",
		dumpCommand: func(name, file string) string {
			return fmt.Sprintf("pg_dump -U %s -d %s -Fc -f %s", databaseUser, name, file)
		},
		loadCommand: func(name, file string) string {
			return fmt.Sprintf("pg_restore -U %s -d %s --clean --if-exists --no-owner --single-transaction %s", databaseUser, name, file)
		},
		readyScript: func(name string) string { return fmt.Sprintf("pg_isready -q -U %s -d %s", databaseUser, name) },
		url: func(host, name string, secrets map[string]string) string {
			return fmt.Sprintf("postgres://%s:%s@%s:5432/%s", databaseUser, secrets["password"], host, name)
		},
	},
	EngineMySQL: {
		image: "mysql", port: 3306, dataPath: "/var/lib/mysql",
		env: func(name string) map[string]string {
			return map[string]string{"MYSQL_USER": databaseUser, "MYSQL_DATABASE": name}
		},
		secretEnv: map[string]string{"MYSQL_PASSWORD": "password", "MYSQL_ROOT_PASSWORD": "password"},
		dumpFile:  "dump.sql",
		dumpCommand: func(name, file string) string {
			return fmt.Sprintf(`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqldump -uroot --single-transaction --routines --triggers --events --add-drop-database --databases %s > %s`, name, file)
		},
		loadCommand: func(name, file string) string {
			return fmt.Sprintf(`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot < %s`, file)
		},
		readyScript: func(string) string {
			return `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqladmin ping -h 127.0.0.1 -uroot --silent`
		},
		url: func(host, name string, secrets map[string]string) string {
			return fmt.Sprintf("mysql://%s:%s@%s:3306/%s", databaseUser, secrets["password"], host, name)
		},
	},
	EngineMariaDB: {
		image: "mariadb", port: 3306, dataPath: "/var/lib/mysql",
		env: func(name string) map[string]string {
			return map[string]string{"MARIADB_USER": databaseUser, "MARIADB_DATABASE": name}
		},
		secretEnv: map[string]string{"MARIADB_PASSWORD": "password", "MARIADB_ROOT_PASSWORD": "password"},
		dumpFile:  "dump.sql",
		dumpCommand: func(name, file string) string {
			return fmt.Sprintf(`MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb-dump -uroot --single-transaction --routines --triggers --events --add-drop-database --databases %s > %s`, name, file)
		},
		loadCommand: func(name, file string) string {
			return fmt.Sprintf(`MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb -uroot < %s`, file)
		},
		readyScript: func(string) string {
			return `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb-admin ping -h 127.0.0.1 -uroot --silent`
		},
		url: func(host, name string, secrets map[string]string) string {
			return fmt.Sprintf("mysql://%s:%s@%s:3306/%s", databaseUser, secrets["password"], host, name)
		},
	},
	EngineMongoDB: {
		image: "mongo", port: 27017, dataPath: "/data/db",
		env: func(name string) map[string]string {
			return map[string]string{"MONGO_INITDB_ROOT_USERNAME": databaseUser, "MONGO_INITDB_DATABASE": name}
		},
		secretEnv: map[string]string{"MONGO_INITDB_ROOT_PASSWORD": "password"},
		dumpFile:  "dump.archive.gz",
		dumpCommand: func(name, file string) string {
			return fmt.Sprintf(`mongodump --quiet -u %s -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --db %s --gzip --archive=%s`, databaseUser, name, file)
		},
		loadCommand: func(name, file string) string {
			return fmt.Sprintf(`mongorestore --quiet -u %s -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --drop --gzip --archive=%s`, databaseUser, file)
		},
		readyScript: func(string) string {
			return fmt.Sprintf(`mongosh --quiet -u %s -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin --eval 'db.adminCommand({ping: 1})'`, databaseUser)
		},
		url: func(host, name string, secrets map[string]string) string {
			return fmt.Sprintf("mongodb://%s:%s@%s:27017/%s?authSource=admin", databaseUser, secrets["password"], host, name)
		},
	},
	EngineRedis: {
		image: "redis", port: 6379, dataPath: "/data",
		env:       func(string) map[string]string { return nil },
		secretEnv: map[string]string{"REDIS_PASSWORD": "password"},
		command:   []string{"sh", "-c", `exec docker-entrypoint.sh redis-server --requirepass "$REDIS_PASSWORD" --appendonly yes`},
		before:    []string{"sh", "-c", `redis-cli --no-auth-warning -a "$REDIS_PASSWORD" SAVE`},
		url: func(host, _ string, secrets map[string]string) string {
			return fmt.Sprintf("redis://:%s@%s:6379", secrets["password"], host)
		},
	},
	EngineValkey: {
		image: "valkey/valkey", port: 6379, dataPath: "/data",
		env:       func(string) map[string]string { return nil },
		secretEnv: map[string]string{"VALKEY_PASSWORD": "password"},
		command:   []string{"sh", "-c", `exec docker-entrypoint.sh valkey-server --requirepass "$VALKEY_PASSWORD" --appendonly yes`},
		before:    []string{"sh", "-c", `valkey-cli --no-auth-warning -a "$VALKEY_PASSWORD" SAVE`},
		url: func(host, _ string, secrets map[string]string) string {
			return fmt.Sprintf("redis://:%s@%s:6379", secrets["password"], host)
		},
	},
	EngineDragonfly: {
		image: "docker.dragonflydb.io/dragonflydb/dragonfly", port: 6379, dataPath: "/data",
		env:       func(string) map[string]string { return nil },
		secretEnv: map[string]string{"DRAGONFLY_PASSWORD": "password"},
		command: []string{"sh", "-c",
			`exec /usr/bin/tini -- entrypoint.sh dragonfly --logtostderr --requirepass "$DRAGONFLY_PASSWORD" --dir /data --dbfilename dump`},
		before: []string{"sh", "-c", `redis-cli --no-auth-warning -a "$DRAGONFLY_PASSWORD" SAVE`},
		url: func(host, _ string, secrets map[string]string) string {
			return fmt.Sprintf("redis://:%s@%s:6379", secrets["password"], host)
		},
	},
	EngineGarnet: {
		// Garnet's image runs as a non-root user that cannot write to a
		// local volume, so it keeps its data in memory.
		image: "ghcr.io/microsoft/garnet", port: 6379,
		env:       func(string) map[string]string { return nil },
		secretEnv: map[string]string{"GARNET_PASSWORD": "password"},
		command:   []string{"sh", "-c", `exec /app/GarnetServer --auth Password --password "$GARNET_PASSWORD"`},
		url: func(host, _ string, secrets map[string]string) string {
			return fmt.Sprintf("redis://:%s@%s:6379", secrets["password"], host)
		},
	},
	EngineClickHouse: {
		image: "clickhouse/clickhouse-server", port: 9000, dataPath: "/var/lib/clickhouse",
		env: func(name string) map[string]string {
			return map[string]string{"CLICKHOUSE_USER": databaseUser, "CLICKHOUSE_DB": clickHouseDatabase(name), "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1"}
		},
		secretEnv: map[string]string{"CLICKHOUSE_PASSWORD": "password"},
		// BACKUP may only write below allowed_path.
		args:        []string{"--", "--backups.allowed_path=" + databaseDumpMount + "/"},
		dumpFile:    "dump",
		prepareDump: "chown clickhouse:clickhouse " + databaseDumpMount,
		dumpCommand: func(name, file string) string {
			return fmt.Sprintf(`clickhouse-client -u %s --password "$CLICKHOUSE_PASSWORD" --query "BACKUP DATABASE `+"`%s`"+` TO File('%s')"`, databaseUser, clickHouseDatabase(name), file)
		},
		loadCommand: func(name, file string) string {
			database := "`" + clickHouseDatabase(name) + "`"
			return fmt.Sprintf(`clickhouse-client -u %s --password "$CLICKHOUSE_PASSWORD" --multiquery --query "DROP DATABASE IF EXISTS %s SYNC; RESTORE DATABASE %s FROM File('%s')"`, databaseUser, database, database, file)
		},
		readyScript: func(string) string {
			return fmt.Sprintf(`clickhouse-client -u %s --password "$CLICKHOUSE_PASSWORD" --query "SELECT 1"`, databaseUser)
		},
		url: func(host, name string, secrets map[string]string) string {
			return fmt.Sprintf("clickhouse://%s:%s@%s:9000/%s", databaseUser, secrets["password"], host, clickHouseDatabase(name))
		},
	},
	EngineLibSQL: {
		image: "ghcr.io/tursodatabase/libsql-server", port: 8080, dataPath: "/var/lib/sqld/data",
		env: func(string) map[string]string {
			return map[string]string{"SQLD_NODE": "primary", "SQLD_DB_PATH": "/var/lib/sqld/data", "SQLD_HTTP_LISTEN_ADDR": "0.0.0.0:8080"}
		},
		secretEnv: map[string]string{"SQLD_AUTH_JWT_KEY": "jwt-key"},
		url: func(host, _ string, secrets map[string]string) string {
			return fmt.Sprintf("http://%s:8080?authToken=%s", host, secrets["token"])
		},
	},
}

// clickHouseDatabase names a ClickHouse database; its identifiers cannot
// contain an unquoted `-`.
func clickHouseDatabase(name string) string { return strings.ReplaceAll(name, "-", "_") }

// DatabaseEngines returns the engines a manifest can declare.
func DatabaseEngines() []string {
	engines := make([]string, 0, len(databaseEngines))
	for engine := range databaseEngines {
		engines = append(engines, engine)
	}
	sort.Strings(engines)
	return engines
}

// DatabaseSpec is what expanding a `databases` entry needs.
type DatabaseSpec struct {
	Engine      string
	Version     string
	Image       string
	Resources   Resources
	Constraints map[string]string
	Backup      *VolumeBackup
}

// DatabaseDataVolume names the volume that holds a database's files.
func DatabaseDataVolume(name string) string { return name + "-data" }

// DatabaseDumpVolume names the volume that holds a dump engine's backups.
func DatabaseDumpVolume(name string) string { return name + "-backup" }

// DatabaseBackupVolume names the volume whose backups hold the database:
// the dump volume for engines backed up with a dump, else the data volume.
func DatabaseBackupVolume(name, engine string) string {
	if databaseEngines[engine].dumpCommand != nil {
		return DatabaseDumpVolume(name)
	}
	return DatabaseDataVolume(name)
}

// DatabaseLoadCommand returns the command that loads the restored dump into
// a running database, or nil when restoring the volume is enough.
func DatabaseLoadCommand(name, engine string) []string {
	spec := databaseEngines[engine]
	if spec.loadCommand == nil {
		return nil
	}
	return []string{"sh", "-c", spec.loadCommand(name, databaseDumpMount+"/"+spec.dumpFile)}
}

// DatabaseReadyCommand returns a command that succeeds once the database
// accepts connections, or nil when the engine has none.
func DatabaseReadyCommand(name, engine string) []string {
	spec := databaseEngines[engine]
	if spec.readyScript == nil {
		return nil
	}
	return []string{"sh", "-c", spec.readyScript(name)}
}

// expandDatabases turns each database into a service of the same name with
// its volumes. Names already taken by the manifest are reported as problems.
func expandDatabases(manifest *Manifest, databases map[string]DatabaseSpec) error {
	var problems []Problem
	add := func(field, message string) { problems = append(problems, Problem{Path: field, Message: message}) }
	if len(databases) > maxDatabases {
		add("databases", fmt.Sprintf("must not define more than %d databases", maxDatabases))
	}
	for _, name := range sortedKeys(databases) {
		spec := databases[name]
		base := "databases." + name
		engine, known := databaseEngines[spec.Engine]
		if !known {
			add(base+".engine", "must be one of "+strings.Join(DatabaseEngines(), ", "))
			continue
		}
		if len(name) > maxDatabaseName {
			add(base, fmt.Sprintf("name must not exceed %d characters", maxDatabaseName))
			continue
		}
		if _, taken := manifest.Services[name]; taken {
			add(base, fmt.Sprintf("a service is already named %q", name))
			continue
		}
		if engine.dataPath == "" && spec.Backup != nil {
			add(base+".backup", "is not supported: "+spec.Engine+" keeps its data in memory")
			continue
		}
		data, dump := DatabaseDataVolume(name), DatabaseDumpVolume(name)
		var volumes []string
		if engine.dataPath != "" {
			volumes = append(volumes, data)
		}
		if spec.Backup != nil && engine.dumpCommand != nil {
			volumes = append(volumes, dump)
		}
		collision := false
		for _, volume := range volumes {
			if _, taken := manifest.Volumes[volume]; taken {
				add(base, fmt.Sprintf("a volume is already named %q", volume))
				collision = true
			}
		}
		if collision {
			continue
		}
		image := spec.Image
		if image == "" {
			image = engine.image + ":" + spec.Version
		}
		secretEnv := make(map[string]string, len(engine.secretEnv))
		for variable, suffix := range engine.secretEnv {
			secretEnv[variable] = name + "-" + suffix
		}
		service := Service{
			Image: image, Command: append([]string(nil), engine.command...), Args: append([]string(nil), engine.args...),
			Replicas: 1, Restart: DefaultRestart,
			Resources:   spec.Resources,
			TCP:         &TCP{Port: engine.port},
			Env:         engine.env(name),
			SecretEnv:   secretEnv,
			Constraints: cloneMap(spec.Constraints),

			StopGrace: "1m",
			Database:  &Database{Engine: spec.Engine, Version: spec.Version},
		}
		if engine.dataPath != "" {
			service.Mounts = append(service.Mounts, Mount{Source: data, Target: engine.dataPath})
		}
		dataVolume := Volume{Driver: "local"}
		if spec.Backup != nil {
			backup := &VolumeBackup{Schedule: spec.Backup.Schedule, Keep: spec.Backup.Keep}
			if engine.dumpCommand != nil {
				file := databaseDumpMount + "/" + engine.dumpFile
				script := fmt.Sprintf("rm -rf %[1]s.tmp && %[2]s && rm -rf %[1]s && mv %[1]s.tmp %[1]s", file, engine.dumpCommand(name, file+".tmp"))
				if engine.prepareDump != "" {
					script = engine.prepareDump + " && " + script
				}
				backup.Before = []string{"sh", "-c", script}
				service.Mounts = append(service.Mounts, Mount{Source: dump, Target: databaseDumpMount})
				manifest.Volumes[dump] = Volume{Driver: "local", Backup: backup}
			} else {
				backup.Before = append([]string(nil), engine.before...)
				dataVolume.Backup = backup
			}
		}
		if engine.dataPath != "" {
			manifest.Volumes[data] = dataVolume
		}
		manifest.Services[name] = service
	}
	if len(problems) != 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// DatabaseSecretNames returns the secrets orch generates for a database:
// the ones its service reads and the ones applications connect with.
func DatabaseSecretNames(name, engine string) []string {
	if engine == EngineLibSQL {
		return []string{name + "-jwt-key", name + "-token", name + "-url"}
	}
	return []string{name + "-password", name + "-url"}
}

// GenerateDatabaseSecrets returns the generated secrets a database is
// missing. existing holds the current values of the names from
// DatabaseSecretNames that are already set; they are never replaced, so a
// value set by hand wins. host is the database's service hostname.
func GenerateDatabaseSecrets(name, engine, host string, existing map[string]string, random io.Reader) (map[string]string, error) {
	spec, known := databaseEngines[engine]
	if !known {
		return nil, fmt.Errorf("unknown database engine %q", engine)
	}
	values := map[string]string{}
	created := map[string]string{}
	set := func(suffix, value string) {
		values[suffix] = value
		created[name+"-"+suffix] = value
	}
	have := func(suffix string) (string, bool) {
		value, ok := existing[name+"-"+suffix]
		return value, ok
	}
	if engine == EngineLibSQL {
		key, keyOK := have("jwt-key")
		token, tokenOK := have("token")
		if keyOK && tokenOK {
			values["jwt-key"], values["token"] = key, token
		} else {
			// The token is only useful with the key it was signed for, so a
			// missing half replaces both.
			public, private, err := ed25519.GenerateKey(random)
			if err != nil {
				return nil, fmt.Errorf("generate libSQL key: %w", err)
			}
			token, err := libSQLToken(private)
			if err != nil {
				return nil, err
			}
			set("jwt-key", base64.RawURLEncoding.EncodeToString(public))
			set("token", token)
		}
	} else if password, ok := have("password"); ok {
		values["password"] = password
	} else {
		password, err := randomPassword(random)
		if err != nil {
			return nil, err
		}
		set("password", password)
	}
	if _, ok := have("url"); !ok || len(created) != 0 {
		set("url", spec.url(host, name, values))
	}
	return created, nil
}

// libSQLToken signs a full-access JWT that sqld accepts with the matching
// public key.
func libSQLToken(private ed25519.PrivateKey) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]string{"a": "rw"})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	signature := ed25519.Sign(private, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// randomPassword returns an alphanumeric password, safe inside a URL.
func randomPassword(random io.Reader) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	password := make([]byte, 0, databasePasswordLen)
	buffer := make([]byte, 64)
	for len(password) < databasePasswordLen {
		if _, err := io.ReadFull(random, buffer); err != nil {
			return "", fmt.Errorf("generate database password: %w", err)
		}
		for _, value := range buffer {
			// 248 is the largest multiple of 62 below 256, so every
			// character is equally likely.
			if value < 248 && len(password) < databasePasswordLen {
				password = append(password, alphabet[int(value)%len(alphabet)])
			}
		}
	}
	return string(password), nil
}

// validateDatabase checks a service's database marker.
func validateDatabase(base string, service Service, add func(string, string)) {
	database := service.Database
	if database == nil {
		return
	}
	if _, known := databaseEngines[database.Engine]; !known {
		add(base+".database.engine", "must be one of "+strings.Join(DatabaseEngines(), ", "))
	}
	if strings.TrimSpace(database.Version) == "" || len(database.Version) > 128 {
		add(base+".database.version", "must contain 1-128 characters")
	}
}
