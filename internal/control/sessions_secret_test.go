package control

import (
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestAssignmentSnapshotCarriesReferencesAndTransientMaterialSeparately(t *testing.T) {
	t.Parallel()
	reference := model.SecretReference{Name: "token", VersionID: "secv_one"}
	registry := &model.RegistryCredentialReference{Host: "registry.example", VersionID: "regv_one", Username: "robot"}
	assignment := model.Assignment{
		NodeID: "node-one", Generation: 1, Digest: "sha256:digest", CreatedAt: time.Now(),
		Allocations: []model.AllocationDesired{{
			ID: "alloc-one", Application: "hello", SecretEnvironment: map[string]model.SecretReference{"TOKEN": reference},
			SecretFiles:        []model.SecretFileDesired{{SecretReference: reference, Target: "/run/secrets/token"}},
			RegistryCredential: registry,
			Rollback:           &model.RollbackSpec{SecretEnvironment: map[string]model.SecretReference{"OLD_TOKEN": reference}, RegistryCredential: registry},
		}},
		CronJobs:  []model.CronJobDesired{{Application: "hello", Job: "backup", SecretEnvironment: map[string]model.SecretReference{"TOKEN": reference}}},
		Materials: []model.SecretMaterial{{Kind: model.SecretMaterialApplication, Application: "hello", Name: "token", VersionID: "secv_one", Value: []byte("sensitive")}},
	}
	message := assignmentSnapshotMessage(assignment)
	allocation := message.GetAllocations()[0]
	if allocation.GetSecretEnvironment()["TOKEN"].GetVersionId() != "secv_one" ||
		allocation.GetSecretFiles()[0].GetTarget() != "/run/secrets/token" ||
		allocation.GetRegistryCredential().GetHost() != "registry.example" ||
		allocation.GetRollback().GetSecretEnvironment()["OLD_TOKEN"].GetName() != "token" ||
		message.GetCronJobs()[0].GetSecretEnvironment()["TOKEN"].GetName() != "token" {
		t.Fatalf("assignment reference mapping is incomplete: %#v", message)
	}
	if len(message.GetSecretMaterials()) != 1 || string(message.GetSecretMaterials()[0].GetValue()) != "sensitive" {
		t.Fatalf("assignment material mapping is incomplete: %#v", message.GetSecretMaterials())
	}
}
