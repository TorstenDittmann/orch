package network

import (
	"context"
	"errors"
	"testing"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestFakeAttachmentIsIdempotentAndOwned(t *testing.T) {
	manager := NewFake()
	config, err := model.FirstNodeNetwork(model.DefaultWorkloadCIDR, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	request := AttachmentRequest{AllocationID: "allocation", ObjectID: "object", SpecDigest: "sha256:spec", NetworkGeneration: 1, NetNS: "/proc/1/ns/net", IfName: "eth0"}
	first, err := manager.Attach(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Attach(context.Background(), request)
	if err != nil || first.Address != second.Address {
		t.Fatalf("second attach = %#v, %v", second, err)
	}
	wrong := request
	wrong.SpecDigest = "sha256:other"
	if _, err := manager.Inspect(context.Background(), wrong); !errors.Is(err, ErrOwnershipMismatch) {
		t.Fatalf("Inspect mismatch error = %v", err)
	}
	if err := manager.Check(context.Background(), request); err != nil {
		t.Fatalf("Check() = %v", err)
	}
	secondRequest := request
	secondRequest.AllocationID = "allocation-two"
	secondRequest.ObjectID = "object-two"
	secondRequest.SpecDigest = "sha256:second"
	if _, err := manager.Attach(context.Background(), secondRequest); err != nil {
		t.Fatal(err)
	}
	if err := manager.GC(context.Background(), []AttachmentRequest{request}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Inspect(context.Background(), secondRequest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect after GC error = %v", err)
	}
	if err := manager.Detach(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Inspect(context.Background(), request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Inspect after detach error = %v", err)
	}
}
