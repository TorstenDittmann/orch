package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const databaseManifest = `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "shop"

services {
  ["web"] {
    image = "example/web:1"
    http = new Http { port = 8080 }
    dependsOn { "main" }
    secretEnv { ["DATABASE_URL"] = "main-url"; ["REDIS_URL"] = "cache-url" }
  }
}

databases {
  ["main"] = new Postgres { backup = new DatabaseBackup { keep = 3 } }
  ["cache"] = new Redis {}
  ["edge"] = new LibSQL { constraints { ["disk"] = "ssd" }; backup = new DatabaseBackup {} }
}
`

func TestLoadDatabases(t *testing.T) {
	manifest := loadManifest(t, databaseManifest)

	main := manifest.Services["main"]
	if main.Image != "postgres:17" || main.Replicas != 1 || main.TCP == nil || main.TCP.Port != 5432 {
		t.Fatalf("main service = %#v", main)
	}
	if !reflect.DeepEqual(main.Database, &Database{Engine: EnginePostgres, Version: "17"}) {
		t.Fatalf("main database = %#v", main.Database)
	}
	if main.Resources.Memory != "256Mi" || main.SecretEnv["POSTGRES_PASSWORD"] != "main-password" || main.Env["POSTGRES_DB"] != "main" {
		t.Fatalf("main service settings = %#v", main)
	}
	wantMounts := []Mount{{Source: "main-backup", Target: "/backup"}, {Source: "main-data", Target: "/var/lib/orch-postgres"}}
	if !reflect.DeepEqual(main.Mounts, wantMounts) {
		t.Fatalf("main mounts = %#v", main.Mounts)
	}
	if manifest.Volumes["main-data"].Backup != nil {
		t.Fatal("the postgres data volume is backed up directly")
	}
	dump := manifest.Volumes["main-backup"].Backup
	if dump == nil || dump.Keep != 3 || dump.Schedule != "0 3 * * *" || !strings.Contains(strings.Join(dump.Before, " "), "pg_dump -U orch -d main -Fc -f /backup/dump.pgc.tmp && mv /backup/dump.pgc.tmp /backup/dump.pgc") {
		t.Fatalf("main dump backup = %#v", dump)
	}
	if DatabaseBackupVolume("main", EnginePostgres) != "main-backup" {
		t.Fatal("postgres backups are not on the dump volume")
	}

	cache := manifest.Services["cache"]
	if cache.Image != "redis:7.4" || len(cache.Mounts) != 1 || manifest.Volumes["cache-data"].Backup != nil {
		t.Fatalf("cache = %#v, volume = %#v", cache, manifest.Volumes["cache-data"])
	}
	if _, exists := manifest.Volumes["cache-backup"]; exists {
		t.Fatal("redis got a dump volume")
	}

	edge := manifest.Services["edge"]
	if edge.Constraints["disk"] != "ssd" || edge.SecretEnv["SQLD_AUTH_JWT_KEY"] != "edge-jwt-key" {
		t.Fatalf("edge = %#v", edge)
	}
	if backup := manifest.Volumes["edge-data"].Backup; backup == nil || len(backup.Before) != 0 {
		t.Fatalf("edge data backup = %#v", backup)
	}

	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseCanonicalJSON(canonical)
	if err != nil {
		t.Fatalf("ParseCanonicalJSON() error = %v", err)
	}
	if !reflect.DeepEqual(parsed.Services["main"].Database, main.Database) {
		t.Fatal("the database marker did not survive the canonical manifest")
	}
}

func TestLoadDatabaseOnlyApplication(t *testing.T) {
	manifest := loadManifest(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"
name = "cache"
databases { ["redis"] = new Redis { version = "8" } }
`)
	if len(manifest.Services) != 1 || manifest.Services["redis"].Image != "redis:8" {
		t.Fatalf("services = %#v", manifest.Services)
	}
}

func TestLoadRejectsDatabaseNameCollisions(t *testing.T) {
	for name, source := range map[string]string{
		"service": `services { ["main"] { image = "example/web:1" } }
databases { ["main"] = new Postgres {} }`,
		"volume": `services { ["web"] { image = "example/web:1" } }
volumes { ["main-data"] {} }
databases { ["main"] = new MySQL {} }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadManifestError(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"
name = "shop"
`+source)
			var validation *ValidationError
			if !errors.As(err, &validation) || !hasProblem(validation, "databases.main") {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestValidateRejectsUnknownDatabaseEngine(t *testing.T) {
	manifest := loadManifest(t, databaseManifest)
	service := manifest.Services["main"]
	service.Database = &Database{Engine: "oracle", Version: "23"}
	manifest.Services["main"] = service
	var validation *ValidationError
	if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, "services.main.database.engine") {
		t.Fatalf("Validate() = %v", validation)
	}
}

func TestGenerateDatabaseSecrets(t *testing.T) {
	created, err := GenerateDatabaseSecrets("main", EnginePostgres, "main.shop.svc.internal", nil, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	password := created["main-password"]
	if len(password) != databasePasswordLen || strings.Trim(password, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789") != "" {
		t.Fatalf("password = %q", password)
	}
	if got, want := created["main-url"], "postgres://orch:"+password+"@main.shop.svc.internal:5432/main"; got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	again, err := GenerateDatabaseSecrets("main", EnginePostgres, "main.shop.svc.internal", created, rand.Reader)
	if err != nil || len(again) != 0 {
		t.Fatalf("existing secrets were regenerated: %v %v", again, err)
	}
	urlOnly, err := GenerateDatabaseSecrets("main", EnginePostgres, "main.shop.svc.internal", map[string]string{"main-password": "hand"}, rand.Reader)
	if err != nil || !reflect.DeepEqual(urlOnly, map[string]string{"main-url": "postgres://orch:hand@main.shop.svc.internal:5432/main"}) {
		t.Fatalf("url from a set password = %v %v", urlOnly, err)
	}
	if _, err := GenerateDatabaseSecrets("main", EnginePostgres, "host", nil, bytes.NewReader(nil)); err == nil {
		t.Fatal("a failing random source produced a password")
	}
}

func TestGenerateLibSQLSecrets(t *testing.T) {
	created, err := GenerateDatabaseSecrets("edge", EngineLibSQL, "edge.shop.svc.internal", nil, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := base64.RawURLEncoding.DecodeString(created["edge-jwt-key"])
	if err != nil || len(public) != ed25519.PublicKeySize {
		t.Fatalf("jwt key = %q", created["edge-jwt-key"])
	}
	parts := strings.Split(created["edge-token"], ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", created["edge-token"])
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
		t.Fatal("the token is not signed by the generated key")
	}
	if got, want := created["edge-url"], "http://edge.shop.svc.internal:8080?authToken="+created["edge-token"]; got != want {
		t.Fatalf("url = %q", got)
	}
	partial, err := GenerateDatabaseSecrets("edge", EngineLibSQL, "edge.shop.svc.internal", map[string]string{"edge-jwt-key": "old"}, rand.Reader)
	if err != nil || partial["edge-jwt-key"] == "" || partial["edge-jwt-key"] == "old" || partial["edge-token"] == "" {
		t.Fatalf("a missing token did not replace the key pair: %v %v", partial, err)
	}
}
