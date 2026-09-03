package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"change.codes/torsten/better-swarm/internal/agent/state"
	"change.codes/torsten/better-swarm/internal/discovery"
	"change.codes/torsten/better-swarm/internal/health"
	"change.codes/torsten/better-swarm/internal/ingress"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/network"
	orchruntime "change.codes/torsten/better-swarm/internal/runtime"
)

const taskStopGrace = 10 * time.Second

type Supervisor struct {
	mu           sync.Mutex
	state        *state.Store
	runtime      orchruntime.Runtime
	prober       health.Prober
	discovery    discovery.Controller
	ingress      ingress.Controller
	dnsStarted   bool
	dnsAddress   string
	deferCleanup bool
	serving      map[string]string
	servingSpecs map[string]string
	accepted     map[string]state.ServingEndpoint
	lastProbes   map[string]time.Time
	routes       []model.RouteStatus
	network      model.NetworkStatus
	now          func() time.Time
	random       io.Reader
}

type SupervisorServices struct {
	Prober    health.Prober
	Discovery discovery.Controller
	Ingress   ingress.Controller
}

func NewSupervisor(cache *state.Store, runtime orchruntime.Runtime, now func() time.Time, sources ...io.Reader) *Supervisor {
	return NewSupervisorWithServices(cache, runtime, now, SupervisorServices{}, sources...)
}

func NewSupervisorWithServices(cache *state.Store, runtime orchruntime.Runtime, now func() time.Time, services SupervisorServices, sources ...io.Reader) *Supervisor {
	if now == nil {
		now = time.Now
	}
	randomSource := io.Reader(rand.Reader)
	if len(sources) != 0 && sources[0] != nil {
		randomSource = sources[0]
	}
	if services.Prober == nil {
		services.Prober = health.NewNetworkProber()
	}
	return &Supervisor{state: cache, runtime: runtime, prober: services.Prober,
		discovery: services.Discovery, ingress: services.Ingress, serving: make(map[string]string),
		servingSpecs: make(map[string]string), accepted: make(map[string]state.ServingEndpoint),
		lastProbes: make(map[string]time.Time),
		now:        now, random: randomSource}
}

// Reconcile converges owned runtime objects to a full assignment. It is
// serialized so snapshot updates and exit polling cannot duplicate work.
func (s *Supervisor) Reconcile(ctx context.Context, assignment model.Assignment) ([]model.AllocationStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serving = make(map[string]string)
	s.servingSpecs = make(map[string]string)
	s.accepted = make(map[string]state.ServingEndpoint)
	s.lastProbes = make(map[string]time.Time)
	s.deferCleanup = assignment.Network.Generation != 0
	networkStatus := model.NetworkStatus{}
	if assignment.Network.Generation != 0 {
		networkStatus = model.NetworkStatus{
			Generation: assignment.Network.Generation, State: model.NetworkPending,
			ClusterCIDR: assignment.Network.ClusterCIDR, NodeSubnet: assignment.Network.NodeSubnet,
			Gateway: assignment.Network.Gateway, DNSState: model.SubsystemPending, IngressState: model.SubsystemPending,
		}
		if err := s.runtime.PrepareNetwork(ctx, assignment.Network); err != nil {
			networkStatus.State = model.NetworkFailed
			networkStatus.Reason = "network_prepare_failed"
			if errors.Is(err, network.ErrCIDRConflict) {
				networkStatus.Reason = "network_cidr_conflict"
			}
			networkStatus.Message = boundedMessage(err.Error())
			networkStatus.DNSState = model.SubsystemBlocked
			networkStatus.IngressState = model.SubsystemBlocked
			s.network = networkStatus
			if err := s.state.SaveNetworkStatus(ctx, networkStatus, s.now().UTC()); err != nil {
				return nil, err
			}
			return networkFailureStatuses(assignment.Allocations, s.now().UTC()), nil
		}
	}
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
		if item.LastProbeAt != nil {
			s.lastProbes[item.ID] = item.LastProbeAt.UTC()
		}
	}
	acceptedServing, err := s.state.ServingEndpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("read durable serving snapshot: %w", err)
	}
	drains, err := s.state.Drains(ctx)
	if err != nil {
		return nil, fmt.Errorf("read durable endpoint drains: %w", err)
	}
	acceptedServing = filterServingEndpoints(acceptedServing, objects)
	for _, endpoint := range acceptedServing {
		s.accepted[orchruntime.ObjectID(endpoint.AllocationID, endpoint.SpecDigest)] = endpoint
	}
	drainByObject := make(map[string]state.Drain, len(drains))
	for _, drain := range drains {
		drainByObject[drain.ObjectID] = drain
	}
	desired := make(map[string]model.AllocationDesired, len(assignment.Allocations))
	objectsByAllocation := make(map[string][]orchruntime.Object)
	for _, allocation := range assignment.Allocations {
		desired[allocation.ID] = allocation
	}
	rollbackApplications := make(map[string]bool)
	for _, allocation := range assignment.Allocations {
		if applicationRollbackState(allocation, stateByID[allocation.ID]) {
			rollbackApplications[allocation.Application] = true
		}
	}
	for _, object := range objects {
		objectsByAllocation[object.AllocationID] = append(objectsByAllocation[object.AllocationID], object)
	}
	for allocationID, owned := range objectsByAllocation {
		if _, ok := desired[allocationID]; ok {
			continue
		}
		if s.deferCleanup {
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
	surges := make(map[string]uint32)
	unavailable := make(map[string]uint32)
	forcedRollback := make(map[string]bool, len(assignment.Allocations))
	newRollback := make(map[string]bool)
	for _, allocation := range assignment.Allocations {
		var exactRunning, obsoleteRunning bool
		for _, object := range objectsByAllocation[allocation.ID] {
			if !object.Running {
				continue
			}
			if object.SpecDigest == allocation.SpecDigest {
				exactRunning = true
			} else {
				obsoleteRunning = true
			}
		}
		if exactRunning && obsoleteRunning {
			surges[allocation.Application]++
		}
		if !exactRunning && !obsoleteRunning {
			unavailable[allocation.Application]++
		} else if exactRunning && !obsoleteRunning && allocation.HTTP != nil {
			previous := stateByID[allocation.ID]
			if previous.SpecDigest != allocation.SpecDigest || previous.HealthStatus != model.HealthReady {
				unavailable[allocation.Application]++
			}
		}
	}
	for _, allocation := range assignment.Allocations {
		owned := objectsByAllocation[allocation.ID]
		if rollbackApplications[allocation.Application] {
			status, forced, err := s.reconcileApplicationRollback(ctx, allocation, owned,
				stateByID[allocation.ID], surges)
			if err != nil {
				return nil, err
			}
			forcedRollback[allocation.ID] = forced
			statuses = append(statuses, status)
			continue
		}
		if needsSurge(allocation, owned) && surges[allocation.Application] >= allocation.MaxSurge {
			status, err := s.rolloutQueuedStatus(ctx, allocation, owned)
			if err != nil {
				return nil, err
			}
			statuses = append(statuses, status)
			continue
		}
		if needsUnavailableSlot(allocation, owned) && unavailable[allocation.Application] >= allocation.MaxUnavailable {
			status, err := s.rolloutQueuedStatus(ctx, allocation, owned, "max_unavailable")
			if err != nil {
				return nil, err
			}
			statuses = append(statuses, status)
			continue
		}
		if needsSurge(allocation, owned) {
			surges[allocation.Application]++
		}
		if needsUnavailableSlot(allocation, owned) {
			unavailable[allocation.Application]++
		}
		if stopFirstDrainPending(allocation, owned, acceptedServing, drainByObject, s.now().UTC()) {
			status, err := s.endpointDrainStatus(ctx, allocation)
			if err != nil {
				return nil, err
			}
			statuses = append(statuses, status)
			continue
		}
		status, err := s.reconcileAllocation(ctx, allocation, owned, stateByID[allocation.ID])
		if err != nil {
			return nil, err
		}
		if statusIsRolledBack(allocation, status) {
			rollbackApplications[allocation.Application] = true
			newRollback[allocation.Application] = true
			forcedRollback[allocation.ID] = true
		}
		statuses = append(statuses, status)
	}
	// A failure discovered late in canonical allocation order must also turn
	// earlier replicas back in the same local transaction boundary. Otherwise
	// the agent could advertise a permanently mixed application generation.
	for index := range statuses {
		allocation := desired[statuses[index].ID]
		if !newRollback[allocation.Application] || forcedRollback[allocation.ID] {
			continue
		}
		status, forced, err := s.reconcileApplicationRollback(ctx, allocation,
			objectsByAllocation[allocation.ID], stateByID[allocation.ID], surges)
		if err != nil {
			return nil, err
		}
		statuses[index] = status
		forcedRollback[allocation.ID] = forced
	}
	for index := range statuses {
		if allocation, ok := desired[statuses[index].ID]; ok && allocation.HTTP != nil {
			statuses[index].DeclaredPort = allocation.HTTP.Port
		}
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
	if assignment.Network.Generation != 0 {
		activated, err := s.reconcileServices(ctx, assignment, statuses, acceptedServing, &networkStatus)
		if err != nil {
			return nil, err
		}
		if activated {
			if err := s.cleanupWithdrawnObjects(ctx, assignment, statuses, objects, acceptedServing); err != nil {
				return nil, err
			}
		}
		remainingOwned, err := s.runtime.ListOwned(ctx)
		if err != nil {
			return nil, fmt.Errorf("count draining runtime objects: %w", err)
		}
		for _, object := range remainingOwned {
			if _, stillDesired := desired[object.AllocationID]; !stillDesired {
				networkStatus.DrainingObjects++
			}
		}
		s.network = networkStatus
		if err := s.state.SaveNetworkStatus(ctx, networkStatus, s.now().UTC()); err != nil {
			return nil, err
		}
	} else {
		s.routes = nil
		s.network = model.NetworkStatus{}
	}
	return statuses, nil
}

func needsSurge(allocation model.AllocationDesired, owned []orchruntime.Object) bool {
	if !allocation.StartBeforeStop || allocation.MaxSurge == 0 {
		return false
	}
	var exact, obsolete bool
	for _, object := range owned {
		if !object.Running {
			continue
		}
		if object.SpecDigest == allocation.SpecDigest {
			exact = true
		} else {
			obsolete = true
		}
	}
	return !exact && obsolete
}

func needsUnavailableSlot(allocation model.AllocationDesired, owned []orchruntime.Object) bool {
	if allocation.StartBeforeStop || allocation.MaxUnavailable == 0 || len(allocation.Mounts) != 0 {
		return false
	}
	var exact, obsolete bool
	for _, object := range owned {
		if !object.Running {
			continue
		}
		if object.SpecDigest == allocation.SpecDigest {
			exact = true
		} else {
			obsolete = true
		}
	}
	return !exact && obsolete
}

func stopFirstDrainPending(allocation model.AllocationDesired, owned []orchruntime.Object,
	accepted []state.ServingEndpoint, drains map[string]state.Drain, now time.Time,
) bool {
	if allocation.StartBeforeStop {
		return false
	}
	acceptedObjects := make(map[string]struct{})
	for _, endpoint := range accepted {
		if endpoint.AllocationID == allocation.ID {
			acceptedObjects[orchruntime.ObjectID(endpoint.AllocationID, endpoint.SpecDigest)] = struct{}{}
		}
	}
	for _, object := range owned {
		if object.Running && object.SpecDigest == allocation.SpecDigest {
			return false
		}
	}
	for _, object := range owned {
		if !object.Running || object.SpecDigest == allocation.SpecDigest {
			continue
		}
		if _, serving := acceptedObjects[object.ID]; serving {
			return true
		}
		if drain, draining := drains[object.ID]; draining && now.Before(drain.Until) {
			return true
		}
	}
	return false
}

func (s *Supervisor) endpointDrainStatus(ctx context.Context, allocation model.AllocationDesired) (model.AllocationStatus, error) {
	status := model.AllocationStatus{
		ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
		Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
		SpecDigest: allocation.SpecDigest, Phase: model.AllocationPending,
		HealthStatus: model.HealthNotConfigured, Reason: "endpoint_draining",
		Message:   "the previous endpoint was withdrawn and is draining before stop-first replacement",
		UpdatedAt: s.now().UTC(),
	}
	if allocation.HTTP != nil {
		status.HealthStatus = model.HealthChecking
	}
	return status, s.persistStatus(ctx, status, nil)
}

func (s *Supervisor) rolloutQueuedStatus(ctx context.Context, allocation model.AllocationDesired, owned []orchruntime.Object, budgets ...string) (model.AllocationStatus, error) {
	budget := "max_surge"
	if len(budgets) != 0 {
		budget = budgets[0]
	}
	status := model.AllocationStatus{
		ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
		Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
		SpecDigest: allocation.SpecDigest, Phase: model.AllocationPending,
		HealthStatus: model.HealthNotConfigured, Reason: "rollout_budget_wait",
		Message: "waiting for the application " + budget + " budget", UpdatedAt: s.now().UTC(),
	}
	if allocation.HTTP != nil {
		status.HealthStatus = model.HealthChecking
	}
	if old := rollbackObject(allocation, owned); old != nil && s.canRetainRollback(allocation, *old) {
		s.setServing(allocation.ID, *old)
		markRollbackActive(&status, allocation, *old, true)
	}
	return status, s.persistStatus(ctx, status, nil)
}

func applicationRollbackState(allocation model.AllocationDesired, previous state.AllocationState) bool {
	if !allocation.AutoRollback || allocation.Rollback == nil || previous.SpecDigest != allocation.SpecDigest {
		return false
	}
	return rollbackStateReason(previous.Reason)
}

func rollbackStateReason(reason string) bool {
	switch reason {
	case "health_gate_rollback", "rollback_revalidating", "rollback_revalidation_failed",
		"rollback_start_failed", "rollback_budget_wait":
		return true
	default:
		return false
	}
}

func statusIsRolledBack(allocation model.AllocationDesired, status model.AllocationStatus) bool {
	return allocation.Rollback != nil && status.Reason == "health_gate_rollback" &&
		status.ActiveSpecDigest == allocation.Rollback.SpecDigest
}

func needsRollbackSurge(allocation model.AllocationDesired, owned []orchruntime.Object) bool {
	if allocation.Rollback == nil || !allocation.StartBeforeStop || allocation.MaxSurge == 0 {
		return false
	}
	var desired, rollback bool
	for _, object := range owned {
		if !object.Running {
			continue
		}
		desired = desired || object.SpecDigest == allocation.SpecDigest
		rollback = rollback || object.SpecDigest == allocation.Rollback.SpecDigest
	}
	return desired && !rollback
}

// reconcileApplicationRollback converges every replica of an application to
// the same controller-authorized prior revision after any one replica trips
// the desired generation's health gate. Healthy desired replicas remain in
// the accepted serving snapshot while their fallback is revalidated.
func (s *Supervisor) reconcileApplicationRollback(ctx context.Context, allocation model.AllocationDesired,
	owned []orchruntime.Object, previous state.AllocationState, surges map[string]uint32,
) (model.AllocationStatus, bool, error) {
	now := s.now().UTC()
	status := model.AllocationStatus{
		ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
		Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
		SpecDigest: allocation.SpecDigest, UpdatedAt: now, RestartCount: previous.RestartCount,
		HealthStatus: model.HealthNotConfigured,
	}
	if previous.SpecDigest == allocation.SpecDigest {
		status.HealthStatus = previous.HealthStatus
		status.Address = previous.Address
		status.ActiveSpecDigest = previous.ActiveSpecDigest
		status.ActiveGeneration = previous.ActiveGeneration
		status.ActiveRevisionID = previous.ActiveRevisionID
		status.ReadyAt = previous.ReadyAt
		status.ConsecutiveSuccesses = previous.ConsecutiveSuccesses
		status.ConsecutiveFailures = previous.ConsecutiveFailures
	}
	if allocation.Rollback == nil || !allocation.AutoRollback {
		return status, false, fmt.Errorf("application rollback requires an authorized fallback")
	}
	delete(s.serving, allocation.ID)
	delete(s.servingSpecs, allocation.ID)

	var desiredObject, fallbackObject *orchruntime.Object
	for index := range owned {
		object := owned[index]
		if !object.Running {
			continue
		}
		switch object.SpecDigest {
		case allocation.SpecDigest:
			if desiredObject != nil {
				return status, false, fmt.Errorf("multiple desired runtime objects match allocation %q: %w", allocation.ID, orchruntime.ErrOwnershipMismatch)
			}
			desiredObject = &object
		case allocation.Rollback.SpecDigest:
			if fallbackObject != nil {
				return status, false, fmt.Errorf("multiple rollback runtime objects match allocation %q: %w", allocation.ID, orchruntime.ErrOwnershipMismatch)
			}
			fallbackObject = &object
		}
	}
	if allocation.Rollback.SpecDigest == allocation.SpecDigest && desiredObject != nil {
		// An unchanged service has one immutable task that represents both
		// application revisions. Reuse it while restoring the explicitly
		// authorized prior revision metadata; no process restart is needed.
		fallbackObject = desiredObject
		desiredObject = nil
	}
	if fallbackObject != nil && s.canRetainRollback(allocation, *fallbackObject) {
		status.Phase = model.AllocationRunning
		status.Address = fallbackObject.Address
		status.ActiveSpecDigest = allocation.Rollback.SpecDigest
		status.ActiveGeneration = allocation.Rollback.ApplicationGeneration
		status.ActiveRevisionID = allocation.Rollback.RevisionID
		status.Reason = "health_gate_rollback"
		status.Message = "the controller-authorized previous revision remains active"
		if allocation.Rollback.HTTP != nil {
			status.HealthStatus = model.HealthReady
			if status.ReadyAt == nil {
				readyAt := now
				status.ReadyAt = &readyAt
			}
		} else {
			status.HealthStatus = model.HealthNotConfigured
		}
		s.setServing(allocation.ID, *fallbackObject)
		return status, true, s.persistStatus(ctx, status, nil)
	}

	if fallbackObject == nil {
		if needsRollbackSurge(allocation, owned) {
			if surges[allocation.Application] >= allocation.MaxSurge {
				status.Phase = model.AllocationPending
				status.Reason = "rollback_budget_wait"
				status.Message = "waiting for the application max_surge budget while restoring the previous revision"
				if desiredObject != nil && s.canRetainServing(*desiredObject) {
					status.Phase = model.AllocationRunning
					status.Address = desiredObject.Address
					status.ActiveSpecDigest = allocation.SpecDigest
					status.ActiveGeneration = allocation.ApplicationGeneration
					status.ActiveRevisionID = allocation.RevisionID
					s.setServing(allocation.ID, *desiredObject)
				}
				return status, true, s.persistStatus(ctx, status, nil)
			}
			surges[allocation.Application]++
		}
		if len(allocation.Mounts) != 0 && desiredObject != nil {
			if err := s.stopAndRemove(ctx, *desiredObject); err != nil {
				return status, false, err
			}
			desiredObject = nil
		}
		fallback := rollbackAllocation(allocation)
		object, err := s.runtime.Ensure(ctx, fallback)
		if err != nil {
			status.Phase = model.AllocationFailed
			status.HealthStatus = model.HealthUnhealthy
			status.Reason = "rollback_start_failed"
			status.Message = "the controller-authorized previous revision could not be started"
			if desiredObject != nil && s.canRetainServing(*desiredObject) {
				status.Phase = model.AllocationRunning
				status.Address = desiredObject.Address
				status.ActiveSpecDigest = allocation.SpecDigest
				status.ActiveGeneration = allocation.ApplicationGeneration
				status.ActiveRevisionID = allocation.RevisionID
				s.setServing(allocation.ID, *desiredObject)
			}
			return status, true, s.persistStatus(ctx, status, nil)
		}
		fallbackObject = &object
	}

	resumeProbe := previous.SpecDigest == allocation.SpecDigest &&
		(previous.Reason == "health_gate_rollback" || previous.Reason == "rollback_revalidating" ||
			previous.Reason == "rollback_revalidation_failed")
	if !resumeProbe {
		status.HealthStatus = model.HealthChecking
		status.ConsecutiveSuccesses = 0
		status.ConsecutiveFailures = 0
		status.ReadyAt = nil
		delete(s.lastProbes, allocation.ID)
	}
	status, err := s.rollbackStatus(ctx, allocation, *fallbackObject, status)
	if err != nil {
		return status, false, err
	}
	if status.HealthStatus != model.HealthReady && desiredObject != nil && s.canRetainServing(*desiredObject) {
		s.setServing(allocation.ID, *desiredObject)
		status.Address = desiredObject.Address
		status.ActiveSpecDigest = allocation.SpecDigest
		status.ActiveGeneration = allocation.ApplicationGeneration
		status.ActiveRevisionID = allocation.RevisionID
		if status.HealthStatus == model.HealthUnhealthy {
			status.Reason = "rollback_revalidation_failed"
			status.Message = "the previous revision failed revalidation; the accepted desired endpoint remains temporarily served"
		} else {
			status.Reason = "rollback_revalidating"
			status.Message = "revalidating the previous revision while the accepted desired endpoint remains served"
		}
		if err := s.persistStatus(ctx, status, nil); err != nil {
			return status, false, err
		}
	}
	return status, true, nil
}

func (s *Supervisor) reconcileAllocation(ctx context.Context, allocation model.AllocationDesired, owned []orchruntime.Object, previous state.AllocationState) (model.AllocationStatus, error) {
	now := s.now().UTC()
	if previous.SpecDigest != allocation.SpecDigest {
		delete(s.lastProbes, allocation.ID)
	}
	status := model.AllocationStatus{
		ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
		Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
		SpecDigest: allocation.SpecDigest, UpdatedAt: now, RestartCount: previous.RestartCount,
		HealthStatus: model.HealthNotConfigured,
	}
	if previous.SpecDigest == allocation.SpecDigest {
		status.HealthStatus = previous.HealthStatus
		status.Address = previous.Address
		status.ActiveSpecDigest = previous.ActiveSpecDigest
		status.ActiveGeneration = previous.ActiveGeneration
		status.ActiveRevisionID = previous.ActiveRevisionID
		status.ReadyAt = previous.ReadyAt
		status.ConsecutiveSuccesses = previous.ConsecutiveSuccesses
		status.ConsecutiveFailures = previous.ConsecutiveFailures
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
		previous.ActiveSpecDigest != "" && previous.ActiveSpecDigest != allocation.SpecDigest && allocation.Rollback != nil &&
		(previous.Reason == "health_gate_rollback" || previous.Reason == "rollback_revalidating") {
		if old := rollbackObject(allocation, obsolete); old != nil {
			return s.rollbackStatus(ctx, allocation, *old, status)
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
		if old := rollbackObject(allocation, obsolete); old != nil && s.canRetainRollback(allocation, *old) {
			s.setServing(allocation.ID, *old)
			markRollbackActive(&status, allocation, *old, true)
		}
		status.Phase = model.AllocationBackoff
		status.Reason = previous.Reason
		status.Message = previous.Message
		status.ExitCode = previous.ExitCode
		return status, nil
	}
	if exact != nil && exact.Running {
		return s.runningStatus(ctx, allocation, *exact, obsolete, status, previous)
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
		old := rollbackObject(allocation, obsolete)
		retainedOld := old != nil && s.canRetainRollback(allocation, *old)
		if retainedOld {
			s.setServing(allocation.ID, *old)
			markRollbackActive(&status, allocation, *old, true)
		}
		return s.failedStart(ctx, allocation, status, retainedOld, restartAttempt, startErr)
	}
	if object.AllocationID != allocation.ID || object.SpecDigest != allocation.SpecDigest || !object.Running {
		return status, fmt.Errorf("runtime returned a mismatched object for allocation %q: %w", allocation.ID, orchruntime.ErrOwnershipMismatch)
	}
	status.ExitCode = nil
	return s.runningStatus(ctx, allocation, object, obsolete, status, previous)
}

func (s *Supervisor) runningStatus(ctx context.Context, allocation model.AllocationDesired, object orchruntime.Object,
	obsolete []orchruntime.Object, status model.AllocationStatus, previous state.AllocationState,
) (model.AllocationStatus, error) {
	status.Phase = model.AllocationRunning
	status.Address = object.Address
	if allocation.HTTP == nil {
		status.HealthStatus = model.HealthNotConfigured
		status.ActiveSpecDigest = allocation.SpecDigest
		status.ActiveGeneration = allocation.ApplicationGeneration
		status.ActiveRevisionID = allocation.RevisionID
		status.Reason = "process_running"
		status.Message = "the exact desired container process is running"
		if object.Address != "" {
			s.setServing(allocation.ID, object)
		}
		if !s.deferCleanup {
			for _, old := range obsolete {
				if err := s.stopAndRemove(ctx, old); err != nil {
					return status, err
				}
			}
		}
		return status, s.persistStatus(ctx, status, nil)
	}

	address, addressErr := netip.ParseAddr(object.Address)
	if lastProbe, ok := s.lastProbes[allocation.ID]; ok && previous.SpecDigest == allocation.SpecDigest &&
		s.now().UTC().Before(lastProbe.Add(health.ProbeInterval)) {
		status.UpdatedAt = previous.UpdatedAt
		status.Reason = previous.Reason
		status.Message = previous.Message
		if status.HealthStatus == model.HealthReady && status.ActiveSpecDigest == allocation.SpecDigest &&
			!rollbackStateReason(previous.Reason) {
			metadataChanged := status.ActiveGeneration != allocation.ApplicationGeneration ||
				status.ActiveRevisionID != allocation.RevisionID
			status.ActiveGeneration = allocation.ApplicationGeneration
			status.ActiveRevisionID = allocation.RevisionID
			s.setServing(allocation.ID, object)
			if metadataChanged {
				status.UpdatedAt = s.now().UTC()
				status.Reason = "readiness_ready"
				status.Message = "the desired process passed its readiness gate"
				return status, s.persistStatus(ctx, status, nil)
			}
		} else if old := rollbackObject(allocation, obsolete); old != nil && s.canRetainRollback(allocation, *old) {
			s.setServing(allocation.ID, *old)
			markRollbackActive(&status, allocation, *old, false)
		}
		return status, nil
	}
	target := health.Target{Address: address, Port: uint16(allocation.HTTP.Port), Path: allocation.HTTP.HealthPath}
	target.Host = strings.TrimSuffix(discovery.ServiceName(allocation.Application, allocation.Service), ".")
	probeErr := addressErr
	if probeErr == nil {
		probeErr = s.prober.Probe(ctx, target)
	}
	probeAt := s.now().UTC()
	s.lastProbes[allocation.ID] = probeAt
	tracker := health.Tracker{
		Status: status.HealthStatus, Successes: status.ConsecutiveSuccesses,
		Failures: status.ConsecutiveFailures, ReadyAt: status.ReadyAt,
	}
	tracker, _ = tracker.Observe(probeErr == nil, s.now().UTC())
	status.HealthStatus = tracker.Status
	status.ConsecutiveSuccesses = tracker.Successes
	status.ConsecutiveFailures = tracker.Failures
	status.ReadyAt = tracker.ReadyAt
	deadline := time.Duration(allocation.StartDeadlineSeconds) * time.Second
	deadlineExceeded := deadline > 0 && !object.StartedAt.IsZero() && !s.now().UTC().Before(object.StartedAt.Add(deadline))
	if tracker.Status == model.HealthReady {
		status.ActiveSpecDigest = allocation.SpecDigest
		status.ActiveGeneration = allocation.ApplicationGeneration
		status.ActiveRevisionID = allocation.RevisionID
		status.Reason = "readiness_ready"
		status.Message = "the desired process passed its readiness gate"
		s.setServing(allocation.ID, object)
		if !s.deferCleanup {
			for _, old := range obsolete {
				if err := s.stopAndRemove(ctx, old); err != nil {
					return status, err
				}
			}
		}
		return status, s.persistStatus(ctx, status, nil)
	}
	if tracker.Status == model.HealthUnhealthy || deadlineExceeded {
		if old := rollbackObject(allocation, obsolete); old != nil && allocation.AutoRollback && s.canRetainRollback(allocation, *old) {
			if !s.deferCleanup {
				if err := s.stopAndRemove(ctx, object); err != nil {
					return status, err
				}
			}
			status.HealthStatus = model.HealthReady
			status.ConsecutiveSuccesses = 0
			status.ConsecutiveFailures = 0
			status.Address = old.Address
			status.ActiveSpecDigest = allocation.Rollback.SpecDigest
			status.ActiveGeneration = allocation.Rollback.ApplicationGeneration
			status.ActiveRevisionID = allocation.Rollback.RevisionID
			readyAt := s.now().UTC()
			status.ReadyAt = &readyAt
			status.Reason = "health_gate_rollback"
			status.Message = "the desired revision failed readiness; the controller-authorized previous revision remains active"
			s.setServing(allocation.ID, *old)
			return status, s.persistStatus(ctx, status, nil)
		}
		if allocation.AutoRollback && allocation.Rollback != nil {
			if len(allocation.Mounts) != 0 || !s.deferCleanup {
				if err := s.stopAndRemove(ctx, object); err != nil {
					return status, err
				}
			}
			fallback := rollbackAllocation(allocation)
			delete(s.lastProbes, allocation.ID)
			rollbackObject, err := s.runtime.Ensure(ctx, fallback)
			if err != nil {
				status.Phase = model.AllocationFailed
				status.HealthStatus = model.HealthUnhealthy
				status.Reason = "rollback_start_failed"
				status.Message = "the desired revision failed readiness and its authorized fallback could not be started"
				return status, s.persistStatus(ctx, status, nil)
			}
			status.HealthStatus = model.HealthChecking
			status.ConsecutiveSuccesses = 0
			status.ConsecutiveFailures = 0
			status.ReadyAt = nil
			return s.rollbackStatus(ctx, allocation, rollbackObject, status)
		}
		if deadlineExceeded {
			if !s.deferCleanup {
				if err := s.stopAndRemove(ctx, object); err != nil {
					return status, err
				}
			}
			status.Phase = model.AllocationFailed
			status.HealthStatus = model.HealthUnhealthy
			status.Reason = "health_deadline_exceeded"
			status.Message = "the desired process did not become ready before its health deadline"
			return status, s.persistStatus(ctx, status, nil)
		}
		status.Reason = "readiness_unhealthy"
		status.Message = "the desired process failed three consecutive readiness probes"
	} else {
		status.Reason = "readiness_checking"
		status.Message = "waiting for two consecutive successful readiness probes"
	}
	if old := rollbackObject(allocation, obsolete); old != nil && s.canRetainRollback(allocation, *old) {
		s.setServing(allocation.ID, *old)
		markRollbackActive(&status, allocation, *old, false)
	}
	return status, s.persistStatus(ctx, status, nil)
}

func rollbackObject(allocation model.AllocationDesired, objects []orchruntime.Object) *orchruntime.Object {
	for index := range objects {
		if objects[index].Running && allocation.Rollback != nil && objects[index].SpecDigest == allocation.Rollback.SpecDigest {
			return &objects[index]
		}
	}
	return nil
}

func rollbackAllocation(allocation model.AllocationDesired) model.AllocationDesired {
	rollback := allocation.Rollback
	return model.AllocationDesired{
		ID: allocation.ID, Application: allocation.Application, Service: allocation.Service, Replica: allocation.Replica,
		ApplicationGeneration: rollback.ApplicationGeneration, RevisionID: rollback.RevisionID,
		SpecDigest: rollback.SpecDigest, Image: rollback.Image,
		Command: append([]string(nil), rollback.Command...), Args: append([]string(nil), rollback.Args...),
		Environment: cloneStringMap(rollback.Environment), RestartPolicy: rollback.RestartPolicy,
		CPUMillis: rollback.CPUMillis, MemoryBytes: rollback.MemoryBytes,
		Mounts:               append([]model.VolumeMount(nil), rollback.Mounts...),
		StartDeadlineSeconds: rollback.StartDeadlineSeconds, AutoRollback: rollback.AutoRollback,
		StartBeforeStop: rollback.StartBeforeStop, HTTP: rollback.HTTP,
		NetworkGeneration: rollback.NetworkGeneration,
	}
}

func (s *Supervisor) rollbackStatus(ctx context.Context, allocation model.AllocationDesired,
	object orchruntime.Object, status model.AllocationStatus,
) (model.AllocationStatus, error) {
	status.Phase = model.AllocationRunning
	status.Address = object.Address
	status.ActiveSpecDigest = allocation.Rollback.SpecDigest
	status.ActiveGeneration = allocation.Rollback.ApplicationGeneration
	status.ActiveRevisionID = allocation.Rollback.RevisionID
	status.Reason = "health_gate_rollback"
	status.Message = "the controller-authorized previous revision remains active"
	if allocation.Rollback.HTTP == nil {
		status.HealthStatus = model.HealthNotConfigured
		s.setServing(allocation.ID, object)
		return status, s.persistStatus(ctx, status, nil)
	}
	if lastProbe, ok := s.lastProbes[allocation.ID]; ok && s.now().UTC().Before(lastProbe.Add(health.ProbeInterval)) {
		if status.HealthStatus == model.HealthReady {
			s.setServing(allocation.ID, object)
		}
		return status, nil
	}
	address, addressErr := netip.ParseAddr(object.Address)
	target := health.Target{Address: address, Port: uint16(allocation.Rollback.HTTP.Port), Path: allocation.Rollback.HTTP.HealthPath}
	target.Host = strings.TrimSuffix(discovery.ServiceName(allocation.Application, allocation.Service), ".")
	probeErr := addressErr
	if probeErr == nil {
		probeErr = s.prober.Probe(ctx, target)
	}
	probeAt := s.now().UTC()
	s.lastProbes[allocation.ID] = probeAt
	tracker := health.Tracker{Status: status.HealthStatus, Successes: status.ConsecutiveSuccesses,
		Failures: status.ConsecutiveFailures, ReadyAt: status.ReadyAt}
	tracker, _ = tracker.Observe(probeErr == nil, s.now().UTC())
	status.HealthStatus = tracker.Status
	status.ConsecutiveSuccesses = tracker.Successes
	status.ConsecutiveFailures = tracker.Failures
	status.ReadyAt = tracker.ReadyAt
	if tracker.Status == model.HealthReady {
		s.setServing(allocation.ID, object)
	} else if tracker.Status == model.HealthUnhealthy {
		status.Phase = model.AllocationFailed
		status.Reason = "rollback_unhealthy"
		status.Message = "the controller-authorized previous revision also failed readiness"
	}
	return status, s.persistStatus(ctx, status, nil)
}

func markRollbackActive(status *model.AllocationStatus, allocation model.AllocationDesired, object orchruntime.Object, useAddress bool) {
	if status == nil || allocation.Rollback == nil || object.SpecDigest != allocation.Rollback.SpecDigest {
		return
	}
	status.ActiveSpecDigest = allocation.Rollback.SpecDigest
	status.ActiveGeneration = allocation.Rollback.ApplicationGeneration
	status.ActiveRevisionID = allocation.Rollback.RevisionID
	if useAddress {
		status.Address = object.Address
	}
}

func (s *Supervisor) setServing(allocationID string, object orchruntime.Object) {
	if object.Address == "" {
		return
	}
	s.serving[allocationID] = object.Address
	s.servingSpecs[allocationID] = object.SpecDigest
}

func (s *Supervisor) canRetainRollback(allocation model.AllocationDesired, object orchruntime.Object) bool {
	if allocation.Rollback == nil || object.SpecDigest != allocation.Rollback.SpecDigest || !object.Running {
		return false
	}
	// A process-only Plan #4 fallback has no network address or serving-state
	// entry. It is still the controller-authorized active process while the
	// first networked replacement is starting.
	if allocation.Rollback.HTTP == nil {
		return true
	}
	return s.canRetainServing(object)
}

func (s *Supervisor) canRetainServing(object orchruntime.Object) bool {
	endpoint, ok := s.accepted[object.ID]
	return ok && object.Running && endpoint.AllocationID == object.AllocationID &&
		endpoint.SpecDigest == object.SpecDigest && endpoint.Address == object.Address
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
	var lastProbeAt *time.Time
	if value, ok := s.lastProbes[status.ID]; ok {
		stamp := value.UTC()
		lastProbeAt = &stamp
	}
	return s.state.SaveAllocationState(ctx, state.AllocationState{
		ID: status.ID, SpecDigest: status.SpecDigest, Phase: status.Phase,
		RestartCount: status.RestartCount, NextAttemptAt: next, ExitCode: status.ExitCode,
		Reason: status.Reason, Message: status.Message, UpdatedAt: status.UpdatedAt,
		HealthStatus: status.HealthStatus, Address: status.Address,
		ActiveSpecDigest: status.ActiveSpecDigest, ActiveGeneration: status.ActiveGeneration,
		ActiveRevisionID: status.ActiveRevisionID, ReadyAt: status.ReadyAt,
		ConsecutiveSuccesses: status.ConsecutiveSuccesses, ConsecutiveFailures: status.ConsecutiveFailures,
		LastProbeAt: lastProbeAt,
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
	object, err := s.activeRuntimeObject(ctx, allocation)
	if err != nil {
		return nil, orchruntime.ErrNotFound
	}
	return s.runtime.TailLogs(ctx, object.ID, lines)
}

func (s *Supervisor) Exec(ctx context.Context, allocation model.AllocationDesired, command []string) (<-chan orchruntime.ExecFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, err := s.activeRuntimeObject(ctx, allocation)
	if err != nil {
		return nil, orchruntime.ErrNotFound
	}
	return s.runtime.Exec(ctx, object.ID, command)
}

func (s *Supervisor) activeRuntimeObject(ctx context.Context, allocation model.AllocationDesired) (orchruntime.Object, error) {
	digest := allocation.SpecDigest
	states, err := s.state.AllocationStates(ctx)
	if err != nil {
		return orchruntime.Object{}, err
	}
	for _, item := range states {
		if item.ID != allocation.ID || item.SpecDigest != allocation.SpecDigest {
			continue
		}
		switch {
		case item.ActiveSpecDigest == allocation.SpecDigest:
			digest = item.ActiveSpecDigest
		case allocation.Rollback != nil && item.ActiveSpecDigest == allocation.Rollback.SpecDigest:
			digest = item.ActiveSpecDigest
		}
		break
	}
	object, err := s.runtime.Inspect(ctx, orchruntime.ObjectID(allocation.ID, digest))
	if err != nil || !object.Running || object.AllocationID != allocation.ID || object.SpecDigest != digest {
		return orchruntime.Object{}, orchruntime.ErrNotFound
	}
	return object, nil
}

func (s *Supervisor) Observations() ([]model.RouteStatus, model.NetworkStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.RouteStatus(nil), s.routes...), s.network
}

func (s *Supervisor) reconcileServices(ctx context.Context, assignment model.Assignment,
	statuses []model.AllocationStatus, accepted []state.ServingEndpoint, networkStatus *model.NetworkStatus,
) (bool, error) {
	now := s.now().UTC()
	dnsReady := false
	if s.discovery == nil {
		networkStatus.DNSState = model.SubsystemNotConfigured
		networkStatus.State = model.NetworkFailed
		networkStatus.Reason = "dns_unavailable"
		networkStatus.Message = "the agent has no workload DNS service"
	} else {
		dnsAddress := net.JoinHostPort(assignment.Network.Gateway, "53")
		if s.dnsStarted && s.dnsAddress != dnsAddress {
			if err := s.discovery.Close(ctx); err != nil {
				networkStatus.State = model.NetworkFailed
				networkStatus.Reason = "dns_restart_failed"
				networkStatus.Message = boundedMessage(err.Error())
				networkStatus.DNSState = model.SubsystemFailed
			}
			s.dnsStarted = false
			s.dnsAddress = ""
		}
		if !s.dnsStarted && networkStatus.DNSState != model.SubsystemFailed {
			if err := s.discovery.Replace(discoverySnapshot(assignment.Allocations, accepted)); err != nil {
				return false, fmt.Errorf("restore durable service discovery snapshot: %w", err)
			}
			if err := s.discovery.Start(dnsAddress); err != nil {
				networkStatus.State = model.NetworkFailed
				networkStatus.Reason = "dns_start_failed"
				networkStatus.Message = boundedMessage(err.Error())
				networkStatus.DNSState = model.SubsystemFailed
			} else {
				s.dnsStarted = true
				s.dnsAddress = dnsAddress
			}
		}
		if s.dnsStarted {
			networkStatus.DNSState = model.SubsystemReady
			dnsReady = true
		} else if networkStatus.DNSState != model.SubsystemFailed {
			networkStatus.DNSState = model.SubsystemPending
		}
	}

	candidate := proposedServingEndpoints(assignment.Allocations, s.serving, s.servingSpecs, now)
	if err := s.state.StageServingEndpoints(ctx, candidate); err != nil {
		return false, fmt.Errorf("stage service discovery snapshot: %w", err)
	}
	routes, pending := desiredRoutes(assignment.Allocations, statuses, s.serving, s.servingSpecs, now)
	activated := dnsReady
	if !dnsReady {
		reason := networkStatus.Reason
		if reason == "" {
			reason = "dns_unavailable"
		}
		message := networkStatus.Message
		if message == "" {
			message = "workload DNS must be ready before routes can be activated"
		}
		unavailable := make([]model.RouteStatus, 0, len(routes)+len(pending))
		for _, route := range routes {
			unavailable = append(unavailable, model.RouteStatus{
				Application: route.Application, Service: route.Service, Domain: route.Domain,
				HTTPS: route.HTTPS, State: model.RouteFailed, CertificateState: "pending",
				UpstreamCount: uint32(len(route.Upstreams)), Reason: reason, Message: message, UpdatedAt: now,
			})
		}
		unavailable = append(unavailable, pending...)
		s.routes = unavailable
		if s.ingress == nil {
			networkStatus.IngressState = model.SubsystemNotConfigured
		} else {
			networkStatus.IngressState = model.SubsystemBlocked
		}
	} else if s.ingress == nil {
		activated = false
		networkStatus.IngressState = model.SubsystemNotConfigured
		networkStatus.State = model.NetworkFailed
		networkStatus.Reason = "caddy_unavailable"
		networkStatus.Message = "the agent has no validated Caddy ingress service"
		unavailable := make([]model.RouteStatus, 0, len(routes)+len(pending))
		for _, route := range routes {
			unavailable = append(unavailable, model.RouteStatus{
				Application: route.Application, Service: route.Service, Domain: route.Domain,
				HTTPS: route.HTTPS, State: model.RouteFailed, UpstreamCount: uint32(len(route.Upstreams)),
				Reason: "caddy_unavailable", Message: networkStatus.Message, UpdatedAt: now,
			})
		}
		for _, route := range pending {
			route.State = model.RouteFailed
			route.Reason = "caddy_unavailable"
			route.Message = networkStatus.Message
			unavailable = append(unavailable, route)
		}
		s.routes = unavailable
	} else {
		observed, err := s.ingress.Apply(ctx, routes, now)
		if err != nil {
			activated = false
			networkStatus.IngressState = model.SubsystemFailed
			networkStatus.Reason = "ingress_apply_failed"
			networkStatus.Message = boundedMessage(err.Error())
			observed = make([]model.RouteStatus, 0, len(routes))
			for _, route := range routes {
				observed = append(observed, model.RouteStatus{
					Application: route.Application, Service: route.Service, Domain: route.Domain,
					HTTPS: route.HTTPS, State: model.RouteFailed, UpstreamCount: uint32(len(route.Upstreams)),
					Reason: "ingress_apply_failed", Message: boundedMessage(err.Error()), UpdatedAt: now,
				})
			}
		}
		s.routes = append(observed, pending...)
		if networkStatus.IngressState != model.SubsystemFailed {
			networkStatus.IngressState = model.SubsystemReady
		}
		for _, route := range observed {
			if route.State == model.RouteFailed {
				networkStatus.IngressState = model.SubsystemFailed
				activated = false
			}
		}
	}
	if activated {
		if err := s.state.CommitServingEndpoints(ctx); err != nil {
			return false, fmt.Errorf("accept service discovery snapshot: %w", err)
		}
		if s.discovery != nil {
			if err := s.discovery.Replace(discoverySnapshot(assignment.Allocations, candidate)); err != nil {
				return false, fmt.Errorf("publish service discovery snapshot: %w", err)
			}
		}
	}
	sort.Slice(s.routes, func(i, j int) bool { return s.routes[i].Domain < s.routes[j].Domain })
	if networkStatus.State != model.NetworkFailed {
		if !dnsReady {
			networkStatus.State = model.NetworkPending
			networkStatus.Reason = "dns_pending"
			networkStatus.Message = "waiting for the workload DNS listener"
		} else if networkStatus.IngressState != model.SubsystemReady {
			networkStatus.State = model.NetworkPending
			networkStatus.Reason = "ingress_pending"
			networkStatus.Message = "waiting for the validated Caddy ingress service"
		} else {
			networkStatus.State = model.NetworkReady
			networkStatus.Reason = "network_ready"
			networkStatus.Message = "workload networking, service discovery, and ingress are ready"
		}
	}
	return activated, nil
}

func proposedServingEndpoints(allocations []model.AllocationDesired, serving, specs map[string]string, now time.Time) []state.ServingEndpoint {
	result := make([]state.ServingEndpoint, 0, len(serving))
	for _, allocation := range allocations {
		address := serving[allocation.ID]
		specDigest := specs[allocation.ID]
		if address == "" || specDigest == "" {
			continue
		}
		result = append(result, state.ServingEndpoint{
			AllocationID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
			Address: address, SpecDigest: specDigest, UpdatedAt: now,
		})
	}
	return result
}

func discoverySnapshot(allocations []model.AllocationDesired, endpoints []state.ServingEndpoint) discovery.Snapshot {
	snapshot := discovery.Snapshot{Known: make(map[string]struct{}), Addresses: make(map[string][]netip.Addr)}
	for _, allocation := range allocations {
		snapshot.Known[discovery.ServiceName(allocation.Application, allocation.Service)] = struct{}{}
	}
	for _, endpoint := range endpoints {
		name := discovery.ServiceName(endpoint.Application, endpoint.Service)
		snapshot.Known[name] = struct{}{}
		if address, err := netip.ParseAddr(endpoint.Address); err == nil && address.Is4() {
			snapshot.Addresses[name] = append(snapshot.Addresses[name], address)
		}
	}
	return snapshot
}

func filterServingEndpoints(endpoints []state.ServingEndpoint, objects []orchruntime.Object) []state.ServingEndpoint {
	running := make(map[string]orchruntime.Object, len(objects))
	for _, object := range objects {
		if object.Running {
			running[object.ID] = object
		}
	}
	result := endpoints[:0]
	for _, endpoint := range endpoints {
		object := running[orchruntime.ObjectID(endpoint.AllocationID, endpoint.SpecDigest)]
		if object.ID != "" && object.Address == endpoint.Address {
			result = append(result, endpoint)
		}
	}
	return result
}

func (s *Supervisor) cleanupWithdrawnObjects(ctx context.Context, assignment model.Assignment,
	statuses []model.AllocationStatus, objects []orchruntime.Object, acceptedBefore []state.ServingEndpoint,
) error {
	now := s.now().UTC()
	desired := make(map[string]model.AllocationDesired, len(assignment.Allocations))
	terminal := make(map[string]bool, len(statuses))
	for _, allocation := range assignment.Allocations {
		desired[allocation.ID] = allocation
	}
	for _, status := range statuses {
		terminal[status.ID] = status.Phase == model.AllocationFailed || status.Phase == model.AllocationStopped ||
			(status.Reason == "health_gate_rollback" && status.ActiveSpecDigest != status.SpecDigest &&
				(status.HealthStatus == model.HealthReady || status.HealthStatus == model.HealthNotConfigured))
	}
	activeObjects := make(map[string]struct{}, len(s.serving))
	for allocationID, address := range s.serving {
		if address != "" && s.servingSpecs[allocationID] != "" {
			activeObjects[orchruntime.ObjectID(allocationID, s.servingSpecs[allocationID])] = struct{}{}
		}
	}
	for _, status := range statuses {
		if status.ActiveSpecDigest == "" {
			continue
		}
		switch status.Phase {
		case model.AllocationRunning, model.AllocationBackoff, model.AllocationPending:
			activeObjects[orchruntime.ObjectID(status.ID, status.ActiveSpecDigest)] = struct{}{}
		}
	}
	previouslyServing := make(map[string]struct{}, len(acceptedBefore))
	for _, endpoint := range acceptedBefore {
		previouslyServing[orchruntime.ObjectID(endpoint.AllocationID, endpoint.SpecDigest)] = struct{}{}
	}
	drains, err := s.state.Drains(ctx)
	if err != nil {
		return fmt.Errorf("read endpoint drains: %w", err)
	}
	drainByObject := make(map[string]state.Drain, len(drains))
	for _, drain := range drains {
		drainByObject[drain.ObjectID] = drain
	}
	remaining := make(map[string]int)
	seenObjects := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		remaining[object.AllocationID]++
		seenObjects[object.ID] = struct{}{}
		if _, active := activeObjects[object.ID]; active {
			if _, draining := drainByObject[object.ID]; draining {
				if err := s.state.DeleteDrain(ctx, object.ID); err != nil {
					return err
				}
			}
			continue
		}
		allocation, isDesired := desired[object.AllocationID]
		if isDesired && object.SpecDigest == allocation.SpecDigest && !terminal[object.AllocationID] {
			continue
		}
		if drain, draining := drainByObject[object.ID]; draining {
			if now.Before(drain.Until) {
				continue
			}
		} else if _, wasServing := previouslyServing[object.ID]; wasServing {
			until, err := s.state.BeginDrain(ctx, state.Drain{
				ObjectID: object.ID, AllocationID: object.AllocationID, SpecDigest: object.SpecDigest,
				Until: now.Add(time.Duration(discovery.RecordTTL) * time.Second),
			})
			if err != nil {
				return fmt.Errorf("persist endpoint drain %q: %w", object.ID, err)
			}
			if now.Before(until) {
				continue
			}
		}
		if err := s.stopAndRemove(ctx, object); err != nil {
			return err
		}
		remaining[object.AllocationID]--
		if err := s.state.DeleteDrain(ctx, object.ID); err != nil {
			return err
		}
	}
	for _, drain := range drains {
		if _, exists := seenObjects[drain.ObjectID]; !exists {
			if err := s.state.DeleteDrain(ctx, drain.ObjectID); err != nil {
				return err
			}
		}
	}
	for allocationID, count := range remaining {
		if _, isDesired := desired[allocationID]; !isDesired && count == 0 {
			if err := s.state.DeleteAllocationState(ctx, allocationID); err != nil {
				return err
			}
		}
	}
	return nil
}

func desiredRoutes(allocations []model.AllocationDesired, statuses []model.AllocationStatus,
	serving, servingSpecs map[string]string, now time.Time,
) ([]ingress.Route, []model.RouteStatus) {
	type serviceKey struct {
		application string
		service     string
	}
	type endpoint struct {
		address string
		http    *model.HTTPDesired
	}
	type serviceState struct {
		desired        *model.HTTPDesired
		rollback       *model.HTTPDesired
		desiredServing bool
		endpoints      []endpoint
	}
	type routeCandidate struct {
		owner serviceKey
		route ingress.Route
	}

	statusCount := make(map[string]int)
	terminalRollbackCount := make(map[string]int)
	for _, status := range statuses {
		statusCount[status.Application]++
		if status.Reason == "health_gate_rollback" {
			terminalRollbackCount[status.Application]++
		}
	}
	terminalRollback := make(map[string]bool)
	for application, count := range statusCount {
		terminalRollback[application] = count > 0 && terminalRollbackCount[application] == count
	}

	desiredOwners := make(map[string]serviceKey)
	services := make(map[serviceKey]*serviceState)
	for _, allocation := range allocations {
		key := serviceKey{application: allocation.Application, service: allocation.Service}
		current := services[key]
		if current == nil {
			current = &serviceState{desired: allocation.HTTP}
			if allocation.Rollback != nil {
				current.rollback = allocation.Rollback.HTTP
			}
			services[key] = current
		}
		if allocation.HTTP != nil {
			for _, domain := range allocation.HTTP.Domains {
				desiredOwners[domain] = key
			}
		}
		address := serving[allocation.ID]
		specDigest := servingSpecs[allocation.ID]
		if address == "" || specDigest == "" {
			continue
		}
		activeHTTP := allocation.HTTP
		if specDigest == allocation.SpecDigest {
			current.desiredServing = true
		} else if allocation.Rollback != nil && specDigest == allocation.Rollback.SpecDigest {
			activeHTTP = allocation.Rollback.HTTP
		} else {
			continue
		}
		current.endpoints = append(current.endpoints, endpoint{address: address, http: activeHTTP})
	}

	candidates := make(map[string][]routeCandidate)
	for key, service := range services {
		contract := service.desired
		if service.rollback != nil && (terminalRollback[key.application] || !service.desiredServing) {
			contract = service.rollback
		}
		if contract == nil {
			continue
		}
		upstreams := make([]string, 0, len(service.endpoints))
		for _, endpoint := range service.endpoints {
			if endpoint.http == nil {
				continue
			}
			upstreams = append(upstreams, net.JoinHostPort(endpoint.address, fmt.Sprintf("%d", endpoint.http.Port)))
		}
		sort.Strings(upstreams)
		upstreams = compactStrings(upstreams)
		if len(upstreams) == 0 {
			continue
		}
		for _, domain := range contract.Domains {
			candidates[domain] = append(candidates[domain], routeCandidate{owner: key, route: ingress.Route{
				Application: key.application, Service: key.service, Domain: domain,
				HTTPS: contract.HTTPS, Upstreams: append([]string(nil), upstreams...),
			}})
		}
	}

	domains := make([]string, 0, len(candidates))
	for domain := range candidates {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	active := make([]ingress.Route, 0, len(domains))
	activeDomains := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		choices := candidates[domain]
		sort.Slice(choices, func(i, j int) bool {
			if choices[i].owner.application != choices[j].owner.application {
				return choices[i].owner.application < choices[j].owner.application
			}
			return choices[i].owner.service < choices[j].owner.service
		})
		selected := choices[0]
		if owner, ok := desiredOwners[domain]; ok {
			for _, choice := range choices {
				if choice.owner == owner {
					selected = choice
					break
				}
			}
		}
		active = append(active, selected.route)
		activeDomains[domain] = struct{}{}
	}

	desiredDomains := make([]string, 0, len(desiredOwners))
	for domain := range desiredOwners {
		if _, ok := activeDomains[domain]; !ok {
			desiredDomains = append(desiredDomains, domain)
		}
	}
	sort.Strings(desiredDomains)
	pending := make([]model.RouteStatus, 0, len(desiredDomains))
	for _, domain := range desiredDomains {
		owner := desiredOwners[domain]
		https := false
		if service := services[owner]; service != nil && service.desired != nil {
			https = service.desired.HTTPS
		}
		pending = append(pending, model.RouteStatus{
			Application: owner.application, Service: owner.service, Domain: domain, HTTPS: https,
			State: model.RoutePending, CertificateState: "pending", Reason: "readiness_pending",
			Message: "the route has no ready upstream", UpdatedAt: now,
		})
	}
	return active, pending
}

func compactStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func networkFailureStatuses(allocations []model.AllocationDesired, now time.Time) []model.AllocationStatus {
	result := make([]model.AllocationStatus, 0, len(allocations))
	for _, allocation := range allocations {
		result = append(result, model.AllocationStatus{
			ID: allocation.ID, Application: allocation.Application, Service: allocation.Service,
			Replica: allocation.Replica, ApplicationGeneration: allocation.ApplicationGeneration,
			SpecDigest: allocation.SpecDigest, Phase: model.AllocationPending,
			HealthStatus: model.HealthNotConfigured, Reason: "network_prepare_failed",
			Message: "the workload network could not be prepared", UpdatedAt: now,
		})
	}
	return result
}

func boundedMessage(value string) string {
	if len(value) > 512 {
		return value[:512]
	}
	return value
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
