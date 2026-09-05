package agent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"change.codes/torsten/better-swarm/internal/agent/state"
	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

func (s *Supervisor) reconcileOneOff(ctx context.Context, job model.CronJobDesired, run state.CronRunState, allocations []model.AllocationStatus, endpoints []model.ServiceEndpoint, now time.Time) (state.CronRunState, error) {
	if run.ID == "" {
		run = state.CronRunState{JobRunStatus: model.JobRunStatus{ID: job.RunID, Application: job.Application, Job: job.Job, SpecDigest: job.SpecDigest, ScheduledAt: *job.RunAt, State: model.JobRunWaiting, Reason: "dependency_wait", Message: "waiting to start", UpdatedAt: now}}
		if err := s.state.SaveCronRunState(ctx, run); err != nil {
			return run, err
		}
	}
	if run.SpecDigest != job.SpecDigest {
		return run, errors.New("one-off run intent changed")
	}
	if run.FinishedAt != nil {
		return run, nil
	}
	// A durable RUNNING marker is written before Ensure. Recovery only inspects
	// that identity; it never repeats an uncertain process start.
	if run.State == model.JobRunRunning && run.ObjectID == "" {
		objects, err := s.runtime.ListOwned(ctx)
		if err != nil {
			return run, err
		}
		for _, object := range objects {
			if object.AllocationID == run.ID {
				run.ObjectID = object.ID
				break
			}
		}
		if run.ObjectID == "" {
			finishCronRun(&run, model.JobRunFailed, "start_outcome_unknown", "the previous start could not be confirmed; an explicit retry is required", now)
		}
	}
	if job.CancelRequested && run.FinishedAt == nil {
		if run.ObjectID != "" {
			if err := s.runtime.Stop(ctx, run.ObjectID, taskStopGrace); err != nil && !errors.Is(err, orchruntime.ErrNotFound) {
				return run, err
			}
		}
		finishCronRun(&run, model.JobRunFailed, "cancelled", "the job was cancelled", now)
	}
	if run.FinishedAt == nil {
		switch run.State {
		case model.JobRunWaiting:
			if !now.Before(run.ScheduledAt.Add(time.Duration(job.StartDeadlineSeconds) * time.Second)) {
				finishCronRun(&run, model.JobRunTimedOut, "start_deadline_exceeded", "the job missed its start deadline", now)
			} else if jobDependenciesAvailable(job, allocations, endpoints) {
				allocation, err := cronRunAllocation(job, run.ID)
				if err != nil {
					return run, err
				}
				run.State, run.Reason, run.Message = model.JobRunRunning, "process_starting", "starting the job process"
				run.ObjectID = orchruntime.ObjectID(run.ID, allocation.SpecDigest)
				run.StartedAt = &now
				run.UpdatedAt = now
				if err := s.state.SaveCronRunState(ctx, run); err != nil {
					return run, err
				}
				startContext, cancel := context.WithDeadline(ctx, run.ScheduledAt.Add(time.Duration(job.StartDeadlineSeconds)*time.Second))
				object, startErr := s.runtime.Ensure(startContext, allocation)
				cancel()
				if startErr == nil {
					run.ObjectID = object.ID
					run.Reason, run.Message = "process_running", "the job process is running"
				} else {
					run.Reason, run.Message = "start_outcome_unknown", "checking whether the job process started"
				}
			}
		case model.JobRunRunning:
			object, err := s.runtime.Inspect(ctx, run.ObjectID)
			if run.StartedAt != nil && !now.Before(run.StartedAt.Add(time.Duration(job.TimeoutSeconds)*time.Second)) {
				if stopErr := s.runtime.Stop(ctx, run.ObjectID, taskStopGrace); stopErr != nil && !errors.Is(stopErr, orchruntime.ErrNotFound) {
					return run, stopErr
				}
				finishCronRun(&run, model.JobRunTimedOut, "timeout_exceeded", "the job exceeded its timeout", now)
			} else if errors.Is(err, orchruntime.ErrNotFound) {
				finishCronRun(&run, model.JobRunFailed, "runtime_object_missing", "the job runtime object is unavailable; no automatic retry", now)
			} else if err != nil {
				return run, err
			} else if !object.Running {
				run.ExitCode = object.ExitCode
				outcome, reason := model.JobRunFailed, "exit_nonzero"
				if object.ExitCode != nil && *object.ExitCode == 0 {
					outcome, reason = model.JobRunSucceeded, "exit_zero"
				}
				finishCronRun(&run, outcome, reason, "the job process exited", now)
			}
		}
	}
	run.UpdatedAt = now
	return run, s.state.SaveCronRunState(ctx, run)
}

// Retain metadata as a tombstone, but bound completed one-off runtime/log objects
// to the newest 100 that are no longer present in the current assignment.
func (s *Supervisor) pruneOneOffLogs(ctx context.Context, runs []state.CronRunState, desired []model.CronJobDesired) error {
	active := make(map[string]bool, len(desired))
	for _, job := range desired {
		active[job.RunID] = true
	}
	expired := make([]state.CronRunState, 0)
	for _, run := range runs {
		if strings.HasPrefix(run.Job, "run-") && run.FinishedAt != nil && !active[run.ID] && run.ObjectID != "" {
			expired = append(expired, run)
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].ScheduledAt.After(expired[j].ScheduledAt) })
	for _, run := range expired[min(100, len(expired)):] {
		object, err := s.runtime.Inspect(ctx, run.ObjectID)
		if err != nil && !errors.Is(err, orchruntime.ErrNotFound) {
			return err
		}
		if err == nil && object.Running {
			continue
		}
		if err := s.runtime.Remove(ctx, run.ObjectID); err != nil && !errors.Is(err, orchruntime.ErrNotFound) {
			return err
		}
		run.ObjectID = ""
		if err := s.state.SaveCronRunState(ctx, run); err != nil {
			return err
		}
	}
	return nil
}
