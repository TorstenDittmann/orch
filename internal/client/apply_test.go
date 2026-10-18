package client

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
)

// sourcelessController behaves like a controller that predates deployment sources.
type sourcelessController struct {
	orchv1alpha1connect.ControlServiceClient
	requests []*orchv1alpha1.ApplyApplicationRequest
}

func (c *sourcelessController) ApplyApplication(_ context.Context, request *connect.Request[orchv1alpha1.ApplyApplicationRequest]) (*connect.Response[orchv1alpha1.ApplyApplicationResponse], error) {
	c.requests = append(c.requests, request.Msg)
	if request.Msg.GetSource() != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request contains unknown fields"))
	}
	return connect.NewResponse(&orchv1alpha1.ApplyApplicationResponse{Name: "hello"}), nil
}

func TestApplyApplicationFromRetriesWithoutSourceOnOlderControllers(t *testing.T) {
	controller := &sourcelessController{}
	client := &Client{service: controller}
	response, err := client.ApplyApplicationFrom(context.Background(), []byte(`{}`), "idempotency-key-one", &orchv1alpha1.DeploymentSource{Repository: "acme/hello"})
	if err != nil || response.GetName() != "hello" {
		t.Fatalf("apply: %+v %v", response, err)
	}
	if len(controller.requests) != 2 || controller.requests[1].GetSource() != nil || controller.requests[1].GetIdempotencyKey() != "idempotency-key-one" {
		t.Fatalf("requests = %+v", controller.requests)
	}
}
