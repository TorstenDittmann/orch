package runtime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

func validationAllocation(t *testing.T, generation uint64, revision, image string) model.AllocationDesired {
	t.Helper()
	allocation := model.AllocationDesired{
		ID: "alloc-one", Application: "game", Service: "server", Replica: 1,
		ApplicationGeneration: generation, RevisionID: revision, Image: image,
		RestartPolicy: "always", CPUMillis: 100, MemoryBytes: 128 << 20, StartDeadlineSeconds: 30,
		AutoRollback: true, NetworkGeneration: 1,
		HTTP: &model.HTTPDesired{Port: 8080, Domains: []string{"game.example.com"}, HealthPath: "/healthz"},
		UDP:  &model.UDPDesired{Port: 7777, NodePort: 7777},
	}
	digest, err := model.AllocationSpecDigest(allocation)
	if err != nil {
		t.Fatal(err)
	}
	allocation.SpecDigest = digest
	return allocation
}

func TestValidateAllocationAcceptsUDPRollback(t *testing.T) {
	previous := validationAllocation(t, 1, "revision-one", "registry.example.com/game@sha256:"+strings.Repeat("1", 64))
	updated := validationAllocation(t, 2, "revision-two", "registry.example.com/game@sha256:"+strings.Repeat("2", 64))
	updated.Rollback = &model.RollbackSpec{
		ApplicationGeneration: previous.ApplicationGeneration, RevisionID: previous.RevisionID,
		SpecDigest: previous.SpecDigest, Image: previous.Image, RestartPolicy: previous.RestartPolicy,
		CPUMillis: previous.CPUMillis, MemoryBytes: previous.MemoryBytes,
		StartDeadlineSeconds: previous.StartDeadlineSeconds, AutoRollback: previous.AutoRollback,
		NetworkGeneration: previous.NetworkGeneration, HTTP: previous.HTTP, UDP: previous.UDP,
	}
	if err := ValidateAllocation(updated); err != nil {
		t.Fatalf("UDP rollback rejected: %v", err)
	}
}

// Every rollback field must reach the reconstructed allocation, or its digest
// no longer matches the spec it was authorized with.
func TestRollbackAllocationCarriesEveryRollbackField(t *testing.T) {
	rollback := model.RollbackSpec{}
	value := reflect.ValueOf(&rollback).Elem()
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		switch field.Kind() {
		case reflect.Bool:
			field.SetBool(true)
		case reflect.Uint32, reflect.Uint64:
			field.SetUint(7)
		case reflect.String:
			field.SetString("set")
		case reflect.Slice:
			field.Set(reflect.MakeSlice(field.Type(), 1, 1))
		case reflect.Map:
			field.Set(reflect.MakeMap(field.Type()))
			field.SetMapIndex(reflect.New(field.Type().Key()).Elem(), reflect.New(field.Type().Elem()).Elem())
		case reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
		default:
			t.Fatalf("rollback field %s has unhandled kind %s", value.Type().Field(index).Name, field.Kind())
		}
	}
	allocation := reflect.ValueOf(rollbackAllocation(model.AllocationDesired{Rollback: &rollback}))
	for index := 0; index < value.NumField(); index++ {
		name := value.Type().Field(index).Name
		target := allocation.FieldByName(name)
		if !target.IsValid() {
			t.Fatalf("allocation has no field for rollback field %s", name)
		}
		if !reflect.DeepEqual(target.Interface(), value.Field(index).Interface()) {
			t.Errorf("rollback field %s was not carried into the rollback allocation", name)
		}
	}
}

func TestValidateAllocationChecksDomainPorts(t *testing.T) {
	for name, test := range map[string]struct {
		ports map[string]uint32
		valid bool
	}{
		"other port":     {map[string]uint32{"game.example.com": 9001}, true},
		"main port":      {map[string]uint32{"game.example.com": 8080}, false},
		"zero port":      {map[string]uint32{"game.example.com": 0}, false},
		"unknown domain": {map[string]uint32{"other.example.com": 9001}, false},
	} {
		t.Run(name, func(t *testing.T) {
			allocation := validationAllocation(t, 1, "revision-one", "registry.example.com/game@sha256:"+strings.Repeat("1", 64))
			allocation.HTTP.DomainPorts = test.ports
			digest, err := model.AllocationSpecDigest(allocation)
			if err != nil {
				t.Fatal(err)
			}
			allocation.SpecDigest = digest
			if err := ValidateAllocation(allocation); (err == nil) != test.valid {
				t.Fatalf("ValidateAllocation() = %v, want valid %v", err, test.valid)
			}
		})
	}
}
