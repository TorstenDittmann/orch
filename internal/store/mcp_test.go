package store

import (
	"context"
	"testing"
	"time"

	"github.com/TorstenDittmann/orch/internal/model"
)

func TestExpiredMCPGrantsArePruned(t *testing.T) {
	ctx := context.Background()
	state := openTestStore(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, grant := range []model.MCPGrant{
		{ID: "0123456789abcdef", Name: "mcp-old", ClientID: "c", Access: model.MCPAccessRead, CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
		{ID: "fedcba9876543210", Name: "mcp-new", ClientID: "c", Access: model.MCPAccessDeploy, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	} {
		if err := state.CreateMCPGrant(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.CreateMCPGrant(ctx, model.MCPGrant{ID: "1111111111111111", Name: "mcp-new", ClientID: "c", Access: model.MCPAccessRead, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != ErrAccessTokenExists {
		t.Fatalf("duplicate name = %v", err)
	}
	if err := state.DeleteExpiredMCPGrants(ctx, now); err != nil {
		t.Fatal(err)
	}
	grants, err := state.MCPGrants(ctx)
	if err != nil || len(grants) != 1 || grants[0].Name != "mcp-new" {
		t.Fatalf("grants after pruning = %+v, %v", grants, err)
	}
}
