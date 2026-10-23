package client

import (
	"context"

	"connectrpc.com/connect"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func (client *Client) SetNotification(ctx context.Context, name, kind, url string, events []string) (*orchv1alpha1.Notification, error) {
	response, err := client.service.SetNotification(ctx, connect.NewRequest(&orchv1alpha1.SetNotificationRequest{
		Name: name, Kind: kind, Url: url, Events: events,
	}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetNotification(), nil
}

func (client *Client) ListNotifications(ctx context.Context) ([]*orchv1alpha1.Notification, error) {
	response, err := client.service.ListNotifications(ctx, connect.NewRequest(&orchv1alpha1.ListNotificationsRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetNotifications(), nil
}

func (client *Client) DeleteNotification(ctx context.Context, name string) (bool, error) {
	response, err := client.service.DeleteNotification(ctx, connect.NewRequest(&orchv1alpha1.DeleteNotificationRequest{Name: name}))
	if err != nil {
		return false, err
	}
	return response.Msg.GetChanged(), nil
}

func (client *Client) TestNotification(ctx context.Context, name string) ([]*orchv1alpha1.TestNotificationResult, error) {
	response, err := client.service.TestNotification(ctx, connect.NewRequest(&orchv1alpha1.TestNotificationRequest{Name: name}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetResults(), nil
}
