package client

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func (c *Client) RunJob(ctx context.Context, req *orchv1alpha1.RunJobRequest) (*orchv1alpha1.OneOffJob, error) {
	r, err := c.service.RunJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetJob(), nil
}
func (c *Client) RunCronJob(ctx context.Context, application, job, key string) (*orchv1alpha1.OneOffJob, error) {
	r, err := c.service.RunCronJob(ctx, connect.NewRequest(&orchv1alpha1.RunCronJobRequest{Application: application, Job: job, IdempotencyKey: key}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetJob(), nil
}
func (c *Client) GetJob(ctx context.Context, id string) (*orchv1alpha1.OneOffJob, error) {
	r, err := c.service.GetJob(ctx, connect.NewRequest(&orchv1alpha1.GetJobRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetJob(), nil
}
func (c *Client) ListJobs(ctx context.Context, app, before string, limit uint32) (*orchv1alpha1.ListJobsResponse, error) {
	r, err := c.service.ListJobs(ctx, connect.NewRequest(&orchv1alpha1.ListJobsRequest{Application: app, BeforeId: before, Limit: limit}))
	if err != nil {
		return nil, err
	}
	return r.Msg, nil
}
func (c *Client) CancelJob(ctx context.Context, id string) (*orchv1alpha1.OneOffJob, error) {
	r, err := c.service.CancelJob(ctx, connect.NewRequest(&orchv1alpha1.CancelJobRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetJob(), nil
}
func (c *Client) RetryJob(ctx context.Context, id, key string) (*orchv1alpha1.OneOffJob, error) {
	r, err := c.service.RetryJob(ctx, connect.NewRequest(&orchv1alpha1.RetryJobRequest{Id: id, IdempotencyKey: key}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetJob(), nil
}
func (c *Client) WaitJob(ctx context.Context, id string) (*orchv1alpha1.OneOffJob, error) {
	for {
		job, err := c.GetJob(ctx, id)
		if err != nil {
			return nil, err
		}
		if job.GetStatus().GetFinishedAt() != nil {
			if job.GetStatus().GetState() == orchv1alpha1.JobRunState_JOB_RUN_STATE_SUCCEEDED {
				return job, nil
			}
			return job, fmt.Errorf("job %s finished: %s (%s)", id, job.GetStatus().GetState(), job.GetStatus().GetReason())
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return job, ctx.Err()
		case <-timer.C:
		}
	}
}
