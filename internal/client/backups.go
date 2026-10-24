package client

import (
	"context"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func (c *Client) SetBackupDestination(ctx context.Context, request *orchv1alpha1.SetBackupDestinationRequest) (*orchv1alpha1.BackupDestination, error) {
	r, err := c.service.SetBackupDestination(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetDestination(), nil
}

func (c *Client) GetBackupDestination(ctx context.Context) (*orchv1alpha1.GetBackupDestinationResponse, error) {
	r, err := c.service.GetBackupDestination(ctx, connect.NewRequest(&orchv1alpha1.GetBackupDestinationRequest{}))
	if err != nil {
		return nil, err
	}
	return r.Msg, nil
}

func (c *Client) RemoveBackupDestination(ctx context.Context) (bool, error) {
	r, err := c.service.RemoveBackupDestination(ctx, connect.NewRequest(&orchv1alpha1.RemoveBackupDestinationRequest{}))
	if err != nil {
		return false, err
	}
	return r.Msg.GetChanged(), nil
}
