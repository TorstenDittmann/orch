package control

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
)

const (
	// nodeUsageStep is the bucket size of the usage history; a day of
	// five-minute buckets is 288 points, enough for a chart.
	nodeUsageStep = 5 * time.Minute
	// nodeUsageFresh is how old a node's newest report may be to count as
	// its current usage.
	nodeUsageFresh = 2 * time.Minute
)

// decorateNodeUsage sets each node's current usage from its newest report.
func (s *service) decorateNodeUsage(ctx context.Context, nodes []model.Node) error {
	latest, err := s.state.LatestNodeUsage(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	for index := range nodes {
		if usage, ok := latest[nodes[index].ID]; ok && now.Sub(usage.ObservedAt) <= nodeUsageFresh {
			nodes[index].Usage = &usage
		}
	}
	return nil
}

func (s *service) GetNodeUsage(ctx context.Context, request *connect.Request[orchv1alpha1.GetNodeUsageRequest]) (*connect.Response[orchv1alpha1.GetNodeUsageResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	node, err := s.state.NodeByName(ctx, request.Msg.GetName())
	if errors.Is(err, store.ErrNodeNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("node was not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read node"))
	}
	history, err := s.state.NodeUsageHistory(ctx, node.ID, s.now().Add(-store.NodeUsageRetention), nodeUsageStep)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read node usage"))
	}
	response := &orchv1alpha1.GetNodeUsageResponse{StepSeconds: uint32(nodeUsageStep / time.Second), Samples: make([]*orchv1alpha1.NodeUsage, 0, len(history))}
	for _, sample := range history {
		response.Samples = append(response.Samples, nodeUsageMessage(sample))
	}
	return connect.NewResponse(response), nil
}

func nodeUsageMessage(usage model.NodeUsage) *orchv1alpha1.NodeUsage {
	return &orchv1alpha1.NodeUsage{
		ObservedAt: timestamppb.New(usage.ObservedAt), CpuUsedMillis: usage.CPUUsedMillis,
		MemoryUsedBytes: usage.MemoryUsedBytes, DiskUsedBytes: usage.DiskUsedBytes, DiskTotalBytes: usage.DiskTotalBytes,
	}
}
