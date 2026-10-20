package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/store"
)

const (
	// autoscaleInterval is how often autoscaled services are evaluated.
	autoscaleInterval = 30 * time.Second
	// autoscaleWindow is how far back replica CPU is averaged, and how long
	// after a change the next scale-up waits so the average covers the new
	// replicas.
	autoscaleWindow = time.Minute
	// autoscaleDownDelay is how long every recommendation must have been
	// lower before a service loses a replica, which keeps short dips from
	// removing capacity a spike will need again.
	autoscaleDownDelay = 5 * time.Minute
	// autoscaleTolerance ignores utilization within 10% of the target.
	autoscaleTolerance = 0.1
	// maxTrackedAllocations bounds the samples kept between evaluations,
	// well above the cluster's replica limit.
	maxTrackedAllocations = 4096
)

type cpuSample struct {
	at     time.Time
	millis uint64
}

type scaleRecommendation struct {
	at       time.Time
	replicas int
}

// serviceAutoscale is what the autoscaler remembers about one service. It
// is not persisted: after a controller restart the service waits for fresh
// samples and a full scale-down delay before it shrinks.
type serviceAutoscale struct {
	since           time.Time
	lastChange      time.Time
	recommendations []scaleRecommendation
	// utilization is the latest average CPU use as a fraction of the
	// request, or negative when there is not enough data.
	utilization float64
	// limited explains why the service runs fewer replicas than it wants.
	limited string
}

// AutoscaleStatus is the current autoscaling view of one service.
type AutoscaleStatus struct {
	Replicas    int
	Min, Max    int
	CPUPercent  int
	Utilization float64
	Limited     string
}

// autoscaler keeps services that set autoscale near their CPU target. It
// reads replica CPU from agent heartbeats, records its choice per service
// outside the application revision, and asks the reconciler to apply it.
type autoscaler struct {
	mu        sync.Mutex
	state     *store.Store
	reconcile func(context.Context) error
	connected func(string) bool
	now       func() time.Time
	samples   map[string][]cpuSample
	services  map[string]*serviceAutoscale
}

func newAutoscaler(state *store.Store, reconcile func(context.Context) error, connected func(string) bool, now func() time.Time) *autoscaler {
	if now == nil {
		now = time.Now
	}
	return &autoscaler{
		state: state, reconcile: reconcile, connected: connected, now: now,
		samples: map[string][]cpuSample{}, services: map[string]*serviceAutoscale{},
	}
}

// Record keeps one allocation's CPU use from a heartbeat.
func (a *autoscaler) Record(allocationID string, millis uint64, at time.Time) {
	if a == nil || allocationID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, known := a.samples[allocationID]; !known && len(a.samples) >= maxTrackedAllocations {
		return
	}
	samples := append(a.samples[allocationID], cpuSample{at: at, millis: millis})
	cutoff := at.Add(-autoscaleWindow)
	for len(samples) > 0 && samples[0].at.Before(cutoff) {
		samples = samples[1:]
	}
	a.samples[allocationID] = samples
}

// AllocationCPU returns an allocation's average CPU use over the window, in
// millicores, and whether there was any sample.
func (a *autoscaler) AllocationCPU(allocationID string) (uint64, bool) {
	if a == nil {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.averageLocked(allocationID, a.now())
}

func (a *autoscaler) averageLocked(allocationID string, now time.Time) (uint64, bool) {
	var total, count uint64
	cutoff := now.Add(-autoscaleWindow)
	for _, sample := range a.samples[allocationID] {
		if !sample.at.Before(cutoff) {
			total += sample.millis
			count++
		}
	}
	if count == 0 {
		return 0, false
	}
	return total / count, true
}

// Status returns the autoscaling view of every autoscaled service of an
// application, keyed by service name.
func (a *autoscaler) Status(application model.Application, scales map[string]store.ServiceScale) map[string]AutoscaleStatus {
	var manifest config.Manifest
	if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
		return nil
	}
	result := map[string]AutoscaleStatus{}
	for name, service := range manifest.Services {
		if service.Autoscale == nil {
			continue
		}
		status := AutoscaleStatus{
			Replicas: scaledReplicas(service, scales[name]), Min: service.Autoscale.Min, Max: service.Autoscale.Max,
			CPUPercent: service.Autoscale.CPUPercent, Utilization: -1,
		}
		if a != nil {
			a.mu.Lock()
			if state := a.services[serviceKey(application.Name, name)]; state != nil {
				status.Utilization, status.Limited = state.utilization, state.limited
			}
			a.mu.Unlock()
		}
		result[name] = status
	}
	return result
}

func (a *autoscaler) run(ctx context.Context) {
	ticker := time.NewTicker(autoscaleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.evaluate(ctx); err != nil && ctx.Err() == nil {
				log.Printf("controller: autoscale: %v", err)
			}
		}
	}
}

// evaluate makes one scaling decision for every autoscaled service and
// reconciles once if any replica count changed.
func (a *autoscaler) evaluate(ctx context.Context) error {
	applications, err := a.state.Applications(ctx, false)
	if err != nil {
		return fmt.Errorf("list applications: %w", err)
	}
	scales, err := a.state.ServiceScales(ctx)
	if err != nil {
		return err
	}
	var capacity *freeCapacity
	now := a.now().UTC()
	active := map[string]bool{}
	changed := false
	for _, summary := range applications {
		var manifest config.Manifest
		if summary.DeletedAt != nil || json.Unmarshal(summary.CanonicalManifest, &manifest) != nil || !autoscales(manifest) {
			continue
		}
		// The list leaves out allocation statuses, which utilization needs.
		application, err := a.state.Application(ctx, summary.Name)
		if errors.Is(err, store.ErrApplicationNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read application %q: %w", summary.Name, err)
		}
		for _, name := range sortedServiceNames(manifest) {
			service := manifest.Services[name]
			if service.Autoscale == nil {
				continue
			}
			active[serviceKey(application.Name, name)] = true
			current := scaledReplicas(service, scales[application.Name][name])
			desired, reason := a.decide(application, name, service, current, now)
			if desired > current {
				if capacity == nil {
					if capacity, err = a.freeCapacity(ctx); err != nil {
						return err
					}
				}
				fit := capacity.fit(service)
				a.setLimited(application.Name, name, "")
				if current+fit < desired {
					a.setLimited(application.Name, name, fmt.Sprintf("no node has room for another replica with %s CPU and %s memory", service.Resources.CPU, service.Resources.Memory))
					desired = current + fit
				}
				capacity.reserve(service, desired-current)
			} else {
				a.setLimited(application.Name, name, "")
			}
			if desired == current {
				continue
			}
			if err := a.state.SetServiceScale(ctx, store.ServiceScale{
				Application: application.Name, Service: name, Replicas: desired, UpdatedAt: now,
			}, current, reason); err != nil {
				return err
			}
			a.mu.Lock()
			a.services[serviceKey(application.Name, name)].lastChange = now
			a.mu.Unlock()
			changed = true
		}
	}
	for application, services := range scales {
		for service := range services {
			if !active[serviceKey(application, service)] {
				if err := a.state.DeleteServiceScale(ctx, application, service); err != nil {
					return err
				}
			}
		}
	}
	a.forget(active, now)
	if changed && a.reconcile != nil {
		return a.reconcile(ctx)
	}
	return nil
}

// decide returns the replica count the service should run and why.
func (a *autoscaler) decide(application model.Application, name string, service config.Service, current int, now time.Time) (int, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := serviceKey(application.Name, name)
	state := a.services[key]
	if state == nil {
		state = &serviceAutoscale{since: now, utilization: -1}
		a.services[key] = state
	}
	request, err := parseCPUMillis(service.Resources.CPU)
	if err != nil || request == 0 {
		return current, ""
	}
	utilization, ok := a.utilizationLocked(application, name, request, now)
	state.utilization = -1
	if ok {
		state.utilization = utilization
	}
	if !ok || !rolloutSettled(application, name) {
		return current, ""
	}
	target := float64(service.Autoscale.CPUPercent) / 100
	desired := recommendReplicas(current, utilization, target, *service.Autoscale)
	state.recommendations = append(state.recommendations, scaleRecommendation{at: now, replicas: desired})
	for len(state.recommendations) > 0 && now.Sub(state.recommendations[0].at) > autoscaleDownDelay {
		state.recommendations = state.recommendations[1:]
	}
	reason := fmt.Sprintf("cpu %d%% of requested, target %d%%", int(math.Round(utilization*100)), service.Autoscale.CPUPercent)
	switch {
	case desired > current:
		if !state.lastChange.IsZero() && now.Sub(state.lastChange) < autoscaleWindow {
			return current, ""
		}
		return desired, reason
	case desired < current:
		if now.Sub(state.since) < autoscaleDownDelay || now.Sub(state.lastChange) < autoscaleDownDelay {
			return current, ""
		}
		highest := desired
		for _, recommendation := range state.recommendations {
			highest = max(highest, recommendation.replicas)
		}
		if highest >= current {
			return current, ""
		}
		return current - 1, reason
	}
	return current, ""
}

// recommendReplicas scales the replica count by how far utilization is from
// the target, at most doubling it in one step and staying within bounds.
func recommendReplicas(current int, utilization, target float64, bounds config.Autoscale) int {
	desired := current
	if ratio := utilization / target; math.Abs(ratio-1) > autoscaleTolerance {
		desired = int(math.Ceil(float64(current) * ratio))
	}
	desired = min(desired, 2*current)
	return min(max(desired, bounds.Min), bounds.Max)
}

// utilizationLocked averages the CPU of the service's ready replicas as a
// fraction of their request. It needs samples from at least half of them.
func (a *autoscaler) utilizationLocked(application model.Application, service string, request uint64, now time.Time) (float64, bool) {
	ready, sampled := 0, 0
	var total uint64
	for _, allocation := range application.Execution.Allocations {
		if allocation.Service != service || allocation.Phase != model.AllocationRunning ||
			(allocation.HealthStatus != model.HealthReady && allocation.HealthStatus != model.HealthNotConfigured) {
			continue
		}
		ready++
		if millis, ok := a.averageLocked(allocation.ID, now); ok {
			total += millis
			sampled++
		}
	}
	if ready == 0 || sampled*2 < ready {
		return 0, false
	}
	return float64(total) / float64(sampled) / float64(request), true
}

// rolloutSettled reports whether the application's current revision is
// fully rolled out for the service, so scaling never races a deploy.
func rolloutSettled(application model.Application, service string) bool {
	execution := application.Execution
	if execution.ObservedGeneration != application.Generation {
		return false
	}
	switch execution.Status {
	case model.ExecutionHealthy, model.ExecutionRunning, model.ExecutionDegraded, model.ExecutionBlocked:
	default:
		return false
	}
	for _, allocation := range execution.Allocations {
		if allocation.Service == service && allocation.ApplicationGeneration != application.Generation {
			return false
		}
	}
	return true
}

func (a *autoscaler) setLimited(application, service, limited string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if state := a.services[serviceKey(application, service)]; state != nil {
		state.limited = limited
	}
}

// forget drops state for services that no longer autoscale and samples of
// allocations that stopped reporting.
func (a *autoscaler) forget(active map[string]bool, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.services {
		if !active[key] {
			delete(a.services, key)
		}
	}
	cutoff := now.Add(-autoscaleWindow)
	for id, samples := range a.samples {
		if len(samples) == 0 || samples[len(samples)-1].at.Before(cutoff) {
			delete(a.samples, id)
		}
	}
}

// freeCapacity is the unreserved CPU and memory of each schedulable node.
// It only estimates what the scheduler will accept, so a scale-up stops at
// what fits instead of leaving replicas blocked.
type freeCapacity struct {
	nodes []freeNode
}

type freeNode struct {
	labels      map[string]string
	cpu, memory uint64
}

func (a *autoscaler) freeCapacity(ctx context.Context) (*freeCapacity, error) {
	nodes, err := a.state.Nodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	capacity := &freeCapacity{}
	for _, node := range nodes {
		if node.Lifecycle != model.NodeLifecycleActive || (a.connected != nil && !a.connected(node.ID)) {
			continue
		}
		cpu, memory, err := a.state.NodeAssignedResources(ctx, node.ID)
		if err != nil {
			return nil, err
		}
		free := freeNode{labels: node.Labels}
		if node.AllocatableCPUMillis > cpu {
			free.cpu = node.AllocatableCPUMillis - cpu
		}
		if node.AllocatableMemoryBytes > memory {
			free.memory = node.AllocatableMemoryBytes - memory
		}
		capacity.nodes = append(capacity.nodes, free)
	}
	return capacity, nil
}

// fit counts how many more replicas of the service fit on matching nodes.
func (c *freeCapacity) fit(service config.Service) int {
	cpu, memory, ok := serviceRequest(service)
	if !ok {
		return 0
	}
	total := 0
	for _, node := range c.nodes {
		if labelsMatch(node.labels, service.Constraints) {
			total += int(min(node.cpu/cpu, node.memory/memory))
		}
	}
	return total
}

// reserve takes count replicas of the service out of the free capacity so
// two services scaling in one pass do not both count the same room.
func (c *freeCapacity) reserve(service config.Service, count int) {
	cpu, memory, ok := serviceRequest(service)
	if !ok {
		return
	}
	sort.SliceStable(c.nodes, func(i, j int) bool { return c.nodes[i].cpu > c.nodes[j].cpu })
	for index := range c.nodes {
		node := &c.nodes[index]
		for count > 0 && labelsMatch(node.labels, service.Constraints) && node.cpu >= cpu && node.memory >= memory {
			node.cpu -= cpu
			node.memory -= memory
			count--
		}
	}
}

func serviceRequest(service config.Service) (uint64, uint64, bool) {
	cpu, err := parseCPUMillis(service.Resources.CPU)
	if err != nil || cpu == 0 {
		return 0, 0, false
	}
	memory, err := parseMemoryBytes(service.Resources.Memory)
	if err != nil || memory == 0 {
		return 0, 0, false
	}
	return cpu, memory, true
}

func labelsMatch(labels, constraints map[string]string) bool {
	for key, value := range constraints {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// scaledReplicas is the replica count an autoscaled service runs: the
// autoscaler's recorded choice within the current bounds, or the minimum.
func scaledReplicas(service config.Service, stored store.ServiceScale) int {
	if service.Autoscale == nil {
		return service.Replicas
	}
	if stored.Replicas == 0 {
		return service.Autoscale.Min
	}
	return min(max(stored.Replicas, service.Autoscale.Min), service.Autoscale.Max)
}

// applyServiceScales rewrites each autoscaled service in the applications'
// manifests to a fixed replica count, so the reconciler plans them like any
// other service. Only the in-memory copies change.
func applyServiceScales(applications []model.Application, scales map[string]map[string]store.ServiceScale) error {
	for index := range applications {
		application := &applications[index]
		if application.DeletedAt != nil {
			continue
		}
		var manifest config.Manifest
		if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
			continue
		}
		rewritten := false
		for name, service := range manifest.Services {
			if service.Autoscale == nil {
				continue
			}
			service.Replicas = scaledReplicas(service, scales[application.Name][name])
			service.Autoscale = nil
			manifest.Services[name] = service
			rewritten = true
		}
		if !rewritten {
			continue
		}
		canonical, err := config.CanonicalJSON(&manifest)
		if err != nil {
			return fmt.Errorf("apply service scales to %q: %w", application.Name, err)
		}
		application.CanonicalManifest = canonical
	}
	return nil
}

// decorateApplicationScaling adds live replica CPU and the autoscaled
// services' replica counts to an application message.
func (s *service) decorateApplicationScaling(ctx context.Context, application model.Application, message *orchv1alpha1.Application) error {
	for _, allocation := range application.Execution.Allocations {
		if millis, ok := s.autoscaler.AllocationCPU(allocation.ID); ok {
			if message.AllocationCpuMillis == nil {
				message.AllocationCpuMillis = map[string]uint64{}
			}
			message.AllocationCpuMillis[allocation.ID] = millis
		}
	}
	var manifest config.Manifest
	if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
		return nil
	}
	if !autoscales(manifest) {
		return nil
	}
	scales, err := s.state.ServiceScales(ctx)
	if err != nil {
		return err
	}
	statuses := s.autoscaler.Status(application, scales[application.Name])
	for _, name := range sortedServiceNames(manifest) {
		status, ok := statuses[name]
		if !ok {
			continue
		}
		item := &orchv1alpha1.ServiceAutoscale{
			Service: name, Replicas: uint32(status.Replicas), Min: uint32(status.Min), Max: uint32(status.Max),
			CpuTargetPercent: uint32(status.CPUPercent), Limited: status.Limited,
		}
		if status.Utilization >= 0 {
			percent := uint32(math.Round(status.Utilization * 100))
			item.CpuPercent = &percent
		}
		message.Autoscale = append(message.Autoscale, item)
	}
	return nil
}

func autoscales(manifest config.Manifest) bool {
	for _, service := range manifest.Services {
		if service.Autoscale != nil {
			return true
		}
	}
	return false
}

func serviceKey(application, service string) string {
	return application + "/" + service
}
