package control

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/config"
)

// Extra capabilities can weaken the node's isolation just as privileged mode
// can. Enforce the same administrator boundary for deploys and rollbacks.
func requireServicePrivileges(ctx context.Context, manifest *config.Manifest) error {
	application, err := requireDeployerOrAdministrator(ctx)
	if err != nil {
		return err
	}
	if application == "" {
		return nil
	}
	if names := manifest.PrivilegedServices(); len(names) != 0 {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("only administrator credentials may deploy privileged services (%s)", strings.Join(names, ", ")))
	}
	if names := manifest.CapabilityServices(); len(names) != 0 {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("only administrator credentials may deploy services with Linux capabilities (%s)", strings.Join(names, ", ")))
	}
	return nil
}
