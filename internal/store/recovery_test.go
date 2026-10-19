package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestRecoveryMigratesOldInitializedSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := embeddedMigrations.ReadFile("migrations/001_control_core.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := fstest.MapFS{"migrations/001_control_core.sql": &fstest.MapFile{Data: migration}}
	if err := applyMigrations(ctx, db, source, testCreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clusters(singleton,id,name,endpoint,generation,created_at) VALUES(1,'cluster-old','test','https://controller.example',1,'2026-09-02T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	state, summary, err := OpenRecoveryCopy(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if summary.SchemaVersion != 1 || summary.TargetSchemaVersion != 16 || summary.Cluster.ID != "cluster-old" || summary.SecretKeyRequired {
		t.Fatalf("legacy recovery: %+v", summary)
	}
	if jobs, err := state.Jobs(ctx, "", "", 100, false); err != nil || len(jobs) != 0 {
		t.Fatalf("new schema missing: %v", err)
	}
}

func TestRecoveryRejectsForeignKeyDamage(t *testing.T) {
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	path := state.Path()
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=OFF; UPDATE events SET cluster_id='missing'"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restored, _, err := OpenRecoveryCopy(ctx, path)
	if err == nil {
		restored.Close()
		t.Fatal("accepted broken foreign keys")
	}
}
