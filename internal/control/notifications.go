package control

import (
	"context"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
)

const maxClusterNotifications = 20

func (s *service) SetNotification(ctx context.Context, request *connect.Request[orchv1alpha1.SetNotificationRequest]) (*connect.Response[orchv1alpha1.SetNotificationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	kind := request.Msg.GetKind()
	if kind != config.NotificationSlack && kind != config.NotificationDiscord && kind != config.NotificationWebhook {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("kind must be slack, discord or webhook"))
	}
	name := request.Msg.GetName()
	if name == "" {
		name = kind
	}
	if err := config.ValidateApplicationName(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must be a lowercase DNS label of at most 63 characters"))
	}
	value := []byte(strings.TrimSpace(request.Msg.GetUrl()))
	if _, err := webhookURL(append([]byte(nil), value...)); err != nil || len(value) > 4096 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("url must be an http or https URL"))
	}
	events := request.Msg.GetEvents()
	if len(events) == 0 {
		events = config.ClusterNotificationEvents
	}
	events = slices.Sorted(slices.Values(events))
	if err := config.ValidateClusterNotificationEvents(events); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	existing, err := s.state.ClusterNotifications(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("list notifications"))
	}
	if len(existing) >= maxClusterNotifications && !slices.ContainsFunc(existing, func(item model.ClusterNotification) bool { return item.Name == name }) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("a cluster can have at most 20 notifications"))
	}
	notification, err := s.state.SetClusterNotification(ctx, s.secretBox, model.ClusterNotification{
		Name: name, Kind: kind, Events: events, CreatedAt: s.now(),
	}, value)
	wipeBytes(value)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("store notification"))
	}
	return connect.NewResponse(&orchv1alpha1.SetNotificationResponse{Notification: notificationMessage(notification)}), nil
}

func (s *service) ListNotifications(ctx context.Context, request *connect.Request[orchv1alpha1.ListNotificationsRequest]) (*connect.Response[orchv1alpha1.ListNotificationsResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	notifications, err := s.state.ClusterNotifications(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("list notifications"))
	}
	response := &orchv1alpha1.ListNotificationsResponse{}
	for _, notification := range notifications {
		response.Notifications = append(response.Notifications, notificationMessage(notification))
	}
	return connect.NewResponse(response), nil
}

func (s *service) DeleteNotification(ctx context.Context, request *connect.Request[orchv1alpha1.DeleteNotificationRequest]) (*connect.Response[orchv1alpha1.DeleteNotificationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	changed, err := s.state.DeleteClusterNotification(ctx, request.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("delete notification"))
	}
	return connect.NewResponse(&orchv1alpha1.DeleteNotificationResponse{Changed: changed}), nil
}

// TestNotification sends a message to one or every notification right away,
// so a wrong URL shows up now rather than during an outage.
func (s *service) TestNotification(ctx context.Context, request *connect.Request[orchv1alpha1.TestNotificationRequest]) (*connect.Response[orchv1alpha1.TestNotificationResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	notifications, err := s.state.ClusterNotifications(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("list notifications"))
	}
	if name := request.Msg.GetName(); name != "" {
		notifications = slices.DeleteFunc(notifications, func(item model.ClusterNotification) bool { return item.Name != name })
		if len(notifications) == 0 {
			return nil, connect.NewError(connect.CodeNotFound, store.ErrClusterNotificationNotFound)
		}
	}
	sender := newNotifier(s.state, s.secretBox, s.cluster.Name)
	sender.retries = nil
	now := s.now().UTC()
	response := &orchv1alpha1.TestNotificationResponse{}
	for _, notification := range notifications {
		notice := notice{Event: "test", Time: now, Payload: []byte(`{}`),
			Text: "[" + s.cluster.Name + "] test notification from orch; " + notification.Name + " is set up"}
		result := &orchv1alpha1.TestNotificationResult{Name: notification.Name}
		if err := sender.sendCluster(ctx, notice, notification); err != nil {
			result.Error = err.Error()
		}
		response.Results = append(response.Results, result)
	}
	return connect.NewResponse(response), nil
}

func notificationMessage(notification model.ClusterNotification) *orchv1alpha1.Notification {
	return &orchv1alpha1.Notification{
		Name: notification.Name, Kind: notification.Kind, Events: notification.Events,
		CreatedAt: timestamppb.New(notification.CreatedAt),
	}
}
