//go:build linux

package containerd

import (
	"context"
	"fmt"

	"github.com/containerd/cgroups/v3/cgroup2/stats"
	tasks "github.com/containerd/containerd/api/services/tasks/v1"

	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
)

// TaskCPU reads the cumulative CPU time of every owned task from its
// cgroup in one containerd call.
func (c *runtimeClient) TaskCPU(ctx context.Context) ([]orchruntime.TaskCPU, error) {
	containers, err := c.client.Containers(ctx)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]string, len(containers))
	for _, container := range containers {
		labels, err := container.Labels(ctx)
		if err != nil {
			return nil, err
		}
		if labels[labelManaged] != "true" || labels[labelCluster] != c.options.ClusterID || labels[labelNode] != c.options.NodeID {
			continue
		}
		owned[container.ID()] = labels[labelAllocation]
	}
	if len(owned) == 0 {
		return nil, nil
	}
	response, err := c.client.TaskService().Metrics(ctx, &tasks.MetricsRequest{})
	if err != nil {
		return nil, fmt.Errorf("read task metrics: %w", err)
	}
	result := make([]orchruntime.TaskCPU, 0, len(response.GetMetrics()))
	for _, metric := range response.GetMetrics() {
		allocationID, ok := owned[metric.GetID()]
		if !ok || metric.GetData() == nil || metric.GetTimestamp() == nil {
			continue
		}
		var cgroup stats.Metrics
		if err := metric.GetData().UnmarshalTo(&cgroup); err != nil || cgroup.GetCPU() == nil {
			continue
		}
		result = append(result, orchruntime.TaskCPU{
			ObjectID: metric.GetID(), AllocationID: allocationID,
			UsageMicros: cgroup.GetCPU().GetUsageUsec(), ObservedAt: metric.GetTimestamp().AsTime(),
		})
	}
	return result, nil
}
