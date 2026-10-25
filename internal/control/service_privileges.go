package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/store"
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

// requirePrivateNetworkChange lets only administrators turn on
// allowPrivateNetwork, which opens the node's private networks to the
// application. Deployers may keep it on once an administrator has.
func (s *service) requirePrivateNetworkChange(ctx context.Context, manifest *config.Manifest) error {
	application, err := requireDeployerOrAdministrator(ctx)
	if err != nil || application == "" || !manifest.AllowPrivateNetwork {
		return err
	}
	current, err := s.state.Application(ctx, manifest.Name)
	if err != nil && !errors.Is(err, store.ErrApplicationNotFound) {
		return connect.NewError(connect.CodeInternal, errors.New("read current application"))
	}
	var deployed config.Manifest
	if err == nil && current.DeletedAt == nil && json.Unmarshal(current.CanonicalManifest, &deployed) == nil && deployed.AllowPrivateNetwork {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("only administrator credentials may turn on allowPrivateNetwork"))
}
