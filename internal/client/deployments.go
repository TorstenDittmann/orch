package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"connectrpc.com/connect"
)

func (c *Client) ListDeployments(ctx context.Context, app string, before uint64, limit uint32) (*orchv1alpha1.ListDeploymentsResponse, error) {
	r, err := c.service.ListDeployments(ctx, connect.NewRequest(&orchv1alpha1.ListDeploymentsRequest{Application: app, BeforeGeneration: before, Limit: limit}))
	if err != nil {
		return nil, err
	}
	return r.Msg, nil
}

func (c *Client) GetDeployment(ctx context.Context, app, revision string) (*orchv1alpha1.Deployment, error) {
	r, err := c.service.GetDeployment(ctx, connect.NewRequest(&orchv1alpha1.GetDeploymentRequest{Application: app, RevisionId: revision}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetDeployment(), nil
}

func (c *Client) CompareDeployments(ctx context.Context, app, from, to string) (*orchv1alpha1.CompareDeploymentsResponse, error) {
	r, err := c.service.CompareDeployments(ctx, connect.NewRequest(&orchv1alpha1.CompareDeploymentsRequest{Application: app, FromRevisionId: from, ToRevisionId: to}))
	if err != nil {
		return nil, err
	}
	return r.Msg, nil
}

func (c *Client) RollbackApplication(ctx context.Context, request *orchv1alpha1.RollbackApplicationRequest) (*orchv1alpha1.ApplyApplicationResponse, error) {
	r, err := c.service.RollbackApplication(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetDeployment(), nil
}

// DeploymentReady never mistakes acceptance or a healthy fallback for success.
// BLOCKED and DEGRADED are recoverable until the caller's deadline.
func DeploymentReady(app *orchv1alpha1.Application, generation uint64, revision string) (bool, error) {
	if app == nil || generation == 0 || revision == "" {
		return false, errors.New("invalid deployment observation")
	}
	if app.GetDeleted() {
		return false, errors.New("application was removed while waiting")
	}
	if app.GetGeneration() > generation {
		return false, fmt.Errorf("deployment generation %d was superseded by generation %d", generation, app.GetGeneration())
	}
	if app.GetGeneration() != generation {
		return false, nil
	}
	if app.GetRevisionId() != revision {
		return false, errors.New("controller returned a different revision for the requested generation")
	}
	if app.GetObservedGeneration() != generation {
		return false, nil
	}
	switch app.GetExecutionStatus() {
	case orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_FAILED, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_ROLLED_BACK:
		return false, fmt.Errorf("deployment %s: %s", app.GetExecutionStatus(), app.GetExecutionReason())
	case orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_REMOVED, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_STOPPING:
		return false, errors.New("application is being removed")
	case orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_RUNNING, orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_HEALTHY:
		return app.GetActiveGeneration() == generation && app.GetActiveRevisionId() == revision, nil
	default:
		return false, nil
	}
}

type ApplicationReader interface {
	GetApplication(context.Context, string) (*orchv1alpha1.Application, error)
}

// WaitDeployment polls a particular accepted generation. Its context owns all
// requests and timers; losing the connection cannot return a successful result.
func WaitDeployment(ctx context.Context, reader ApplicationReader, name string, generation uint64, revision string, interval time.Duration, observe func(*orchv1alpha1.Application)) (*orchv1alpha1.Application, error) {
	if interval <= 0 {
		return nil, errors.New("poll interval must be positive")
	}
	var latest *orchv1alpha1.Application
	for {
		if err := ctx.Err(); err != nil {
			return latest, err
		}
		value, err := reader.GetApplication(ctx, name)
		if err != nil {
			return latest, fmt.Errorf("watch deployment: %w", err)
		}
		if value.GetName() != name {
			return latest, errors.New("controller returned an inconsistent application")
		}
		latest = value
		if observe != nil {
			observe(value)
		}
		ready, err := DeploymentReady(value, generation, revision)
		if err != nil || ready {
			return latest, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return latest, ctx.Err()
		case <-timer.C:
		}
	}
}
