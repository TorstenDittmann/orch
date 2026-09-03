package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"change.codes/torsten/better-swarm/internal/agent/state"
	"change.codes/torsten/better-swarm/internal/model"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

const taskStopGrace = 10 * time.Second

type Supervisor struct {
	mu      sync.Mutex
	state   *state.Store
	runtime orchruntime.Runtime
	now     func() time.Time
	random  io.Reader
}

func NewSupervisor(cache *state.Store, runtime orchruntime.Runtime, now func() time.Time, sources ...io.Reader) *Supervisor {
	if now == nil {
		now = time.Now
	}
	randomSource := io.Reader(rand.Reader)
	if len(sources) != 0 && sources[0] != nil {
		randomSource = sources[0]
	}
	return &Supervisor{state: cache, runtime: runtime, now: now, random: randomSource}
}

// Reconcile converges owned runtime objects to a full assignment. It is
// serialized so snapshot updates and exit polling cannot duplicate work.
func (s *Supervisor) Reconcile(ctx context.Context, assignment model.Assignment) ([]model.AllocationStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects, err := s.runtime.ListOwned(ctx)
	if err != nil {
		return nil, fmt.Errorf("list owned runtime objects: %w", err)
	}
	states, err := s.state.AllocationStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("read allocation restart state: %w", err)
	}
	stateByID := make(map[string]state.AllocationState, len(states))
	for _, item := range states {
		stateByID[item.ID] = item
	}
	desired := make(map[string]model.AllocationDesired, len(assignment.Allocations))
	objectsByAllocation := make(map[string][]orchruntime.Object)
	for _, allocation := range assignment.Allocations {
		desired[allocation.ID] = allocation
	}
	for _, object := range objects {
		objectsByAllocation[object.AllocationID] = append(objectsByAllocation[object.AllocationID], object)
	}
	for allocationID, owned := range objectsByAllocation {
		if _, ok := desired[allocationID]; ok {
			continue
		}
		for _, object := range owned {
			if err := s.stopAndRemove(ctx, object); err != nil {
				return nil, err
			}
		}
		if err := s.state.DeleteAllocationState(ctx, allocationID); err != nil {
			return nil, err
		}
	}

	statuses := make([]model.AllocationStatus, 0, len(assignment.Allocations))
	for _, allocation := range assignment.Allocations {
		status, err := s.reconcileAllocation(ctx, allocation, objectsByAllocation[allocation.ID], stateByID[allocation.ID])
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Application != statuses[j].Application {
			return statuses[i].Application < statuses[j].Application
		}
		if statuses[i].Service != statuses[j].Service {
			return statuses[i].Service < statuses[j].Service
		}
		return statuses[i].Replica < statuses[j].Replica
	})
	return statuses, nil
}

func (s *Supervisor) reconcileAllocation(ctx context.Context, allocation model.AllocationDesired, owned []orchruntime.Object, previous state.AllocationState) (model.AllocationStatus, error) {
	now := s.now().UTC()
	status := model.AllocationStatus{
		ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
		Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
		SpecDigest: allocation.SpecDigest, UpdatedAt: now, RestartCount: previous.RestartCount,
	}
	var exact *orchruntime.Object
	var obsolete []orchruntime.Object
	for index := range owned {
		object := owned[index]
		if object.SpecDigest == allocation.SpecDigest {
			if exact != nil {
				return status, fmt.Errorf("multiple runtime objects match allocation %q: %w", allocation.ID, orchruntime.ErrOwnershipMismatch)
			}
			exact = &object
		} else {
			obsolete = append(obsolete, object)
		}
	}
	if exact == nil && previous.SpecDigest == allocation.SpecDigest &&
		(previous.Phase == model.AllocationStopped || previous.Phase == model.AllocationFailed) {
		status.Phase = previous.Phase
		status.Reason = previous.Reason
		status.Message = previous.Message
		status.ExitCode = previous.ExitCode
		return status, nil
	}
	if exact == nil && previous.SpecDigest == allocation.SpecDigest && previous.NextAttemptAt != nil && now.Before(*previous.NextAttemptAt) {
		status.Phase = model.AllocationBackoff
		status.Reason = previous.Reason
		status.Message = previous.Message
		status.ExitCode = previous.ExitCode
		return status, nil
	}
	if exact != nil && exact.Running {
		for _, object := range obsolete {
			if err := s.stopAndRemove(ctx, object); err != nil {
				return status, err
			}
		}
		status.Phase = model.AllocationRunning
		status.Reason = "process_running"
		status.Message = "the exact desired container process is running; health is not evaluated"
		return status, s.persistStatus(ctx, status, nil)
	}

	restartAttempt := exact != nil && !exact.Running
	if restartAttempt {
		status.ExitCode = exact.ExitCode
		if !shouldRestart(allocation.RestartPolicy, exact.ExitCode) {
			status.Phase = model.AllocationStopped
			status.Reason = "restart_policy_complete"
			status.Message = "the process exited and its restart policy does not restart it"
			return status, s.persistStatus(ctx, status, nil)
		}
		if previous.SpecDigest == allocation.SpecDigest && previous.NextAttemptAt != nil && now.Before(*previous.NextAttemptAt) {
			status.Phase = model.AllocationBackoff
			status.Reason = previous.Reason
			status.Message = previous.Message
			return status, nil
		}
		status.RestartCount++
	}

	if exact == nil && len(obsolete) != 0 && !allocation.StartBeforeStop {
		for _, object := range obsolete {
			if err := s.stopAndRemove(ctx, object); err != nil {
				return status, err
			}
		}
		obsolete = nil
	}
	status.Phase = model.AllocationStarting
	status.Reason = "process_starting"
	status.Message = "creating the exact desired container process"
	if err := s.persistStatus(ctx, status, nil); err != nil {
		return status, err
	}
	deadline := time.Duration(allocation.StartDeadlineSeconds) * time.Second
	if deadline <= 0 {
		deadline = 2 * time.Minute
	}
	startContext, cancel := context.WithTimeout(ctx, deadline)
	object, startErr := s.runtime.Ensure(startContext, allocation)
	cancel()
	if startErr != nil {
		return s.failedStart(ctx, allocation, status, len(obsolete) != 0, restartAttempt, startErr)
	}
	if object.AllocationID != allocation.ID || object.SpecDigest != allocation.SpecDigest || !object.Running {
		return status, fmt.Errorf("runtime returned a mismatched object for allocation %q: %w", allocation.ID, orchruntime.ErrOwnershipMismatch)
	}
	for _, old := range obsolete {
		if err := s.stopAndRemove(ctx, old); err != nil {
			return status, err
		}
	}
	status.Phase = model.AllocationRunning
	status.Reason = "process_running"
	status.Message = "the exact desired container process is running; health is not evaluated"
	status.ExitCode = nil
	return status, s.persistStatus(ctx, status, nil)
}

func (s *Supervisor) failedStart(ctx context.Context, allocation model.AllocationDesired, status model.AllocationStatus, retainedOld, restartAttempt bool, startErr error) (model.AllocationStatus, error) {
	status.UpdatedAt = s.now().UTC()
	if allocation.RestartPolicy == "never" {
		status.Phase = model.AllocationFailed
		status.Reason = "process_start_failed"
		status.Message = "the process could not be started and restart policy is never"
		return status, s.persistStatus(ctx, status, nil)
	}
	if !restartAttempt {
		status.RestartCount++
	}
	delay, err := s.restartDelay(status.RestartCount)
	if err != nil {
		return status, fmt.Errorf("generate restart backoff jitter: %w", err)
	}
	next := status.UpdatedAt.Add(delay)
	status.Phase = model.AllocationBackoff
	status.Reason = "process_start_backoff"
	if retainedOld && allocation.AutoRollback {
		status.Reason = "replacement_rollback_retained"
		status.Message = "replacement failed; the prior process-running revision was retained"
	} else {
		status.Message = "process start failed; waiting before retry"
	}
	if err := s.persistStatus(ctx, status, &next); err != nil {
		return status, err
	}
	_ = startErr // Runtime details are intentionally not persisted in status.
	return status, nil
}

func (s *Supervisor) persistStatus(ctx context.Context, status model.AllocationStatus, next *time.Time) error {
	return s.state.SaveAllocationState(ctx, state.AllocationState{
		ID: status.ID, SpecDigest: status.SpecDigest, Phase: status.Phase,
		RestartCount: status.RestartCount, NextAttemptAt: next, ExitCode: status.ExitCode,
		Reason: status.Reason, Message: status.Message, UpdatedAt: status.UpdatedAt,
	})
}

func (s *Supervisor) stopAndRemove(ctx context.Context, object orchruntime.Object) error {
	if object.Running {
		if err := s.runtime.Stop(ctx, object.ID, taskStopGrace); err != nil && !errors.Is(err, orchruntime.ErrNotFound) {
			return fmt.Errorf("stop owned runtime object %q: %w", object.ID, err)
		}
	}
	if err := s.runtime.Remove(ctx, object.ID); err != nil && !errors.Is(err, orchruntime.ErrNotFound) {
		return fmt.Errorf("remove owned runtime object %q: %w", object.ID, err)
	}
	return nil
}

func (s *Supervisor) TailLogs(ctx context.Context, allocation model.AllocationDesired, lines uint32) ([]orchruntime.LogFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, err := s.runtime.Inspect(ctx, orchruntime.ObjectID(allocation.ID, allocation.SpecDigest))
	if err != nil || !object.Running {
		return nil, orchruntime.ErrNotFound
	}
	return s.runtime.TailLogs(ctx, object.ID, lines)
}

func (s *Supervisor) Exec(ctx context.Context, allocation model.AllocationDesired, command []string) (<-chan orchruntime.ExecFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, err := s.runtime.Inspect(ctx, orchruntime.ObjectID(allocation.ID, allocation.SpecDigest))
	if err != nil || !object.Running {
		return nil, orchruntime.ErrNotFound
	}
	return s.runtime.Exec(ctx, object.ID, command)
}

func shouldRestart(policy string, exitCode *int32) bool {
	switch policy {
	case "always":
		return true
	case "on-failure":
		return exitCode == nil || *exitCode != 0
	default:
		return false
	}
}

func restartBackoff(restarts uint32) time.Duration {
	if restarts > 6 {
		restarts = 6
	}
	return time.Second * time.Duration(1<<restarts)
}

func (s *Supervisor) restartDelay(restarts uint32) (time.Duration, error) {
	backoff := restartBackoff(restarts)
	jitter, err := reconnectJitter(s.random, backoff/2)
	if err != nil {
		return 0, err
	}
	return backoff + jitter, nil
}
