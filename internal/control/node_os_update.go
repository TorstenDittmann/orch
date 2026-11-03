package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
)

// osUpdateOperationTimeout covers the package upgrade on the agent, which
// gives up after 45 minutes.
const osUpdateOperationTimeout = 50 * time.Minute

// UpdateNodeOS upgrades the packages of a cordoned or draining node through
// its agent. When the agent reboots the host, the restore lifecycle is kept
// in the store and applied once the agent is back, so the node returns to
// service even if the caller, or the controller sharing the host, goes away
// during the reboot. Otherwise it is applied before returning.
func (s *service) UpdateNodeOS(ctx context.Context, request *connect.Request[orchv1alpha1.UpdateNodeOSRequest]) (*connect.Response[orchv1alpha1.UpdateNodeOSResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	restore := lifecycleModel(request.Msg.GetRestoreLifecycle())
	if restore == model.NodeLifecycleDraining {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("restore_lifecycle must be ACTIVE, CORDONED or unspecified"))
	}
	node, err := s.state.NodeByName(ctx, request.Msg.GetName())
	if errors.Is(err, store.ErrNodeNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("node %q was not found", request.Msg.GetName()))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read node"))
	}
	if node.Lifecycle != model.NodeLifecycleCordoned && node.Lifecycle != model.NodeLifecycleDraining {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("node %s is %s; drain or cordon it first", node.Name, node.Lifecycle))
	}
	session := s.sessions.session(node.ID)
	if session == nil || !s.sessions.Connected(node.ID) {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("node %s is not connected", node.Name))
	}
	if osUpdates, _, _ := s.sessions.host(node.ID); !osUpdates {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the agent on %s cannot run OS updates: it needs root, systemd and apt-get or dnf", node.Name))
	}
	session.mu.Lock()
	bootID := session.bootID
	session.mu.Unlock()
	// Recorded first: the agent may reboot right after it answers.
	if restore != "" {
		if err := s.state.SetNodeOSUpdateRestore(ctx, node.ID, restore, bootID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("record node OS update"))
		}
	}
	rebooting, output, err := s.runNodeOSUpdate(ctx, node.ID)
	if rebooting {
		return connect.NewResponse(&orchv1alpha1.UpdateNodeOSResponse{Rebooting: true, BootId: bootID, Output: output}), nil
	}
	if errors.Is(err, errAgentDisconnected) {
		// The agent went away mid-update; the recorded lifecycle is applied
		// when it reconnects.
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("node %s disconnected during the OS update; it returns to %s when its agent reconnects", node.Name, restoreName(restore)))
	}
	// No reboot follows, so the node goes back to service now, also when the
	// upgrade failed: the host is unchanged or still runs fine.
	if restore != "" {
		if clearErr := s.state.SetNodeOSUpdateRestore(ctx, node.ID, "", ""); clearErr != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("clear node OS update"))
		}
		if restoreErr := restoreNodeLifecycle(ctx, s.state, s.reconciler, s.now(), node.Name, restore); restoreErr != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("return node %s to %s: %w", node.Name, restore, restoreErr))
		}
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&orchv1alpha1.UpdateNodeOSResponse{BootId: bootID, Output: output}), nil
}

func (s *service) runNodeOSUpdate(ctx context.Context, nodeID string) (rebooting bool, output string, err error) {
	operationContext, cancel := context.WithTimeout(ctx, osUpdateOperationTimeout)
	defer cancel()
	_, frames, cancelOperation, err := s.sessions.beginOperation(operationContext, nodeID, func(requestID string) *orchv1alpha1.ConnectResponse {
		return &orchv1alpha1.ConnectResponse{Message: &orchv1alpha1.ConnectResponse_OsUpdate{OsUpdate: &orchv1alpha1.OSUpdateCommand{RequestId: requestID}}}
	})
	if err != nil {
		if errors.Is(err, errAgentDisconnected) {
			return false, "", connect.NewError(connect.CodeUnavailable, err)
		}
		return false, "", operationConnectError(err)
	}
	defer cancelOperation()
	select {
	case <-operationContext.Done():
		return false, "", connect.NewError(connect.CodeDeadlineExceeded, errors.New("OS update timed out"))
	case result, ok := <-frames:
		if !ok {
			return false, "", errAgentDisconnected
		}
		switch value := result.GetResult().(type) {
		case *orchv1alpha1.AgentOperationResult_OsUpdate:
			return value.OsUpdate.GetRebooting(), value.OsUpdate.GetOutput(), nil
		case *orchv1alpha1.AgentOperationResult_Error:
			return false, "", agentOperationError(value.Error)
		default:
			return false, "", connect.NewError(connect.CodeInternal, errors.New("agent returned the wrong operation frame"))
		}
	}
}

// restoreAfterOSUpdate returns a node to the lifecycle its OS update asked
// for once its agent runs again after the reboot.
func (s *agentService) restoreAfterOSUpdate(ctx context.Context, nodeID, bootID string) {
	name, lifecycle, err := s.state.TakeNodeOSUpdateRestore(ctx, nodeID, bootID)
	if err == nil && lifecycle != "" {
		err = restoreNodeLifecycle(ctx, s.state, s.reconciler, s.now(), name, lifecycle)
		if err == nil {
			slog.Info("node is back after its OS update", "node", name, "lifecycle", lifecycle)
		}
	}
	if err != nil {
		slog.Warn("restore node lifecycle after OS update failed", "node", nodeID, "error", err)
	}
}

func restoreNodeLifecycle(ctx context.Context, state *store.Store, reconciler *Reconciler, now time.Time, name, lifecycle string) error {
	digest := sha256.Sum256([]byte("os-update\x00" + name + "\x00" + lifecycle))
	result, err := state.UpdateNode(ctx, model.UpdateNode{
		NodeName: name, SetLifecycle: true, Lifecycle: lifecycle,
		// Each restore is its own change, never a replay of an earlier one.
		IdempotencyKey: fmt.Sprintf("os-update-%s-%d", hex.EncodeToString(digest[:8]), now.UnixNano()), RequestHash: hex.EncodeToString(digest[:]), UpdatedAt: now.UTC(),
	})
	if err != nil {
		return err
	}
	// The agent's Hello reconciles right after this; the RPC path does it here.
	if result.Changed && reconciler != nil {
		return reconciler.Reconcile(ctx)
	}
	return nil
}

func restoreName(lifecycle string) string {
	if lifecycle == "" {
		return "its current lifecycle"
	}
	return lifecycle
}
