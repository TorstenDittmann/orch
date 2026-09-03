package control

import (
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestAllocationStatusMessageIncludesDeclaredPort(t *testing.T) {
	t.Parallel()
	message := allocationStatusMessage(model.AllocationStatus{
		ID: "allocation-one", Application: "hello", Service: "web", Replica: 1,
		UpdatedAt: time.Now().UTC(), DeclaredPort: 8080,
	})
	if message.GetDeclaredPort() != 8080 {
		t.Fatalf("declared port = %d, want 8080", message.GetDeclaredPort())
	}
}
