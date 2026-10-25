package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	orchruntime "github.com/TorstenDittmann/orch/internal/runtime"
	"github.com/TorstenDittmann/orch/internal/store"
	"google.golang.org/protobuf/proto"
)

var errJobPlacement = errors.New("no connected compatible node has capacity for this job and its service constraints")

// cronRunServicePrefix marks a one-off run of a scheduled job in the job's
// service name. A slash cannot appear in a service name, so the two never
// collide.
const cronRunServicePrefix = "cron/"

func jobRequestHash(message proto.Message) string {
	data, _ := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func jobID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "job_" + hex.EncodeToString(sum[:16])
}

func (s *service) RunJob(ctx context.Context, req *connect.Request[orchv1alpha1.RunJobRequest]) (*connect.Response[orchv1alpha1.RunJobResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(req.Msg); err != nil {
		return nil, err
	}
	if !idempotencyKeyPattern.MatchString(req.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key is required"))
	}
	hash := jobRequestHash(req.Msg)
	if job, found, err := s.state.ReplayJob(ctx, req.Msg.GetIdempotencyKey(), hash); err != nil {
		return nil, jobError(err)
	} else if found {
		return connect.NewResponse(&orchv1alpha1.RunJobResponse{Job: jobMessage(job)}), nil
	}
	if s.reconciler == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("job reconciliation is unavailable"))
	}
	s.reconciler.mu.Lock()
	job, err := s.reconciler.createOneOff(ctx, req.Msg.GetApplication(), req.Msg.GetService(), req.Msg.GetExpectedRevisionId(), req.Msg.GetCommand(), req.Msg.GetArgs(), req.Msg.GetTimeoutSeconds(), "manual", "", req.Msg.GetIdempotencyKey(), hash, nil, "")
	s.reconciler.mu.Unlock()
	if err != nil {
		return nil, jobError(err)
	}
	if err := s.reconciler.Reconcile(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("publish job assignment; retry using the same idempotency key"))
	}
	return connect.NewResponse(&orchv1alpha1.RunJobResponse{Job: jobMessage(job)}), nil
}

// RunCronJob starts a scheduled job now with exactly the settings its schedule
// uses, on the node that owns it, so a job that reads a local volume runs next
// to that volume.
func (s *service) RunCronJob(ctx context.Context, req *connect.Request[orchv1alpha1.RunCronJobRequest]) (*connect.Response[orchv1alpha1.RunCronJobResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(req.Msg); err != nil {
		return nil, err
	}
	if !idempotencyKeyPattern.MatchString(req.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key is required"))
	}
	hash := jobRequestHash(req.Msg)
	if job, found, err := s.state.ReplayJob(ctx, req.Msg.GetIdempotencyKey(), hash); err != nil {
		return nil, jobError(err)
	} else if found {
		return connect.NewResponse(&orchv1alpha1.RunCronJobResponse{Job: jobMessage(job)}), nil
	}
	if s.reconciler == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("job reconciliation is unavailable"))
	}
	s.reconciler.mu.Lock()
	job, err := func() (model.Job, error) {
		scheduled, node, err := s.reconciler.assignedCronJob(ctx, req.Msg.GetApplication(), req.Msg.GetJob())
		if err != nil {
			return model.Job{}, err
		}
		return s.reconciler.createOneOff(ctx, scheduled.Application, cronRunServicePrefix+scheduled.Job, scheduled.RevisionID, nil, nil, scheduled.TimeoutSeconds, "manual", "", req.Msg.GetIdempotencyKey(), hash, &scheduled, node)
	}()
	s.reconciler.mu.Unlock()
	if err != nil {
		return nil, jobError(err)
	}
	if err := s.reconciler.Reconcile(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("publish job assignment; retry using the same idempotency key"))
	}
	return connect.NewResponse(&orchv1alpha1.RunCronJobResponse{Job: jobMessage(job)}), nil
}

// assignedCronJob finds the scheduled job as it is currently assigned and the
// node that owns it.
func (r *Reconciler) assignedCronJob(ctx context.Context, application, name string) (model.CronJobDesired, string, error) {
	nodes, err := r.state.Nodes(ctx)
	if err != nil {
		return model.CronJobDesired{}, "", err
	}
	for _, node := range nodes {
		assignment, err := r.state.CurrentAssignment(ctx, node.ID)
		if err != nil {
			continue
		}
		for _, job := range assignment.CronJobs {
			if job.RunID == "" && job.Application == application && job.Job == name {
				return job, node.ID, nil
			}
		}
	}
	return model.CronJobDesired{}, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the cron job is not scheduled on any node yet; check orch app status"))
}
func (s *service) GetJob(ctx context.Context, req *connect.Request[orchv1alpha1.GetJobRequest]) (*connect.Response[orchv1alpha1.GetJobResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(req.Msg); err != nil {
		return nil, err
	}
	job, err := s.state.Job(ctx, req.Msg.GetId())
	if err != nil {
		return nil, jobError(err)
	}
	return connect.NewResponse(&orchv1alpha1.GetJobResponse{Job: jobMessage(job)}), nil
}
func (s *service) ListJobs(ctx context.Context, req *connect.Request[orchv1alpha1.ListJobsRequest]) (*connect.Response[orchv1alpha1.ListJobsResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(req.Msg); err != nil {
		return nil, err
	}
	limit := req.Msg.GetLimit()
	if limit == 0 {
		limit = 20
	}
	if limit > 100 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("limit must be 1-100"))
	}
	jobs, err := s.state.Jobs(ctx, req.Msg.GetApplication(), req.Msg.GetBeforeId(), int(limit), false)
	if err != nil {
		return nil, jobError(err)
	}
	response := &orchv1alpha1.ListJobsResponse{}
	for _, job := range jobs {
		response.Jobs = append(response.Jobs, jobMessage(job))
	}
	if len(jobs) == int(limit) {
		response.NextBeforeId = jobs[len(jobs)-1].ID
	}
	return connect.NewResponse(response), nil
}
func (s *service) CancelJob(ctx context.Context, req *connect.Request[orchv1alpha1.CancelJobRequest]) (*connect.Response[orchv1alpha1.CancelJobResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(req.Msg); err != nil {
		return nil, err
	}
	job, err := s.state.CancelJob(ctx, req.Msg.GetId())
	if err != nil {
		return nil, jobError(err)
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish cancellation; cancellation remains requested"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.CancelJobResponse{Job: jobMessage(job)}), nil
}
func (s *service) RetryJob(ctx context.Context, req *connect.Request[orchv1alpha1.RetryJobRequest]) (*connect.Response[orchv1alpha1.RetryJobResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(req.Msg); err != nil {
		return nil, err
	}
	if !idempotencyKeyPattern.MatchString(req.Msg.GetIdempotencyKey()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key is required"))
	}
	hash := jobRequestHash(req.Msg)
	if job, found, err := s.state.ReplayJob(ctx, req.Msg.GetIdempotencyKey(), hash); err != nil {
		return nil, jobError(err)
	} else if found {
		return connect.NewResponse(&orchv1alpha1.RetryJobResponse{Job: jobMessage(job)}), nil
	}
	source, err := s.state.Job(ctx, req.Msg.GetId())
	if err != nil {
		return nil, jobError(err)
	}
	if source.Status.State == model.JobRunSucceeded {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("successful jobs cannot be retried; start a new manual run"))
	}
	if source.Status.FinishedAt == nil {
		return nil, jobError(store.ErrJobActive)
	}
	if s.reconciler == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("job reconciliation is unavailable"))
	}
	s.reconciler.mu.Lock()
	job, err := s.reconciler.createOneOff(ctx, source.Template.Application, source.Service, source.Template.RevisionID, source.Template.Command, source.Template.Args, source.Template.TimeoutSeconds, source.Kind, source.ID, req.Msg.GetIdempotencyKey(), hash, &source.Template, retryNode(source))
	s.reconciler.mu.Unlock()
	if err != nil {
		return nil, jobError(err)
	}
	if err := s.reconciler.Reconcile(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("publish retry; retry using the same idempotency key"))
	}
	return connect.NewResponse(&orchv1alpha1.RetryJobResponse{Job: jobMessage(job)}), nil
}
func jobMessage(job model.Job) *orchv1alpha1.OneOffJob {
	return &orchv1alpha1.OneOffJob{Id: job.ID, Application: job.Template.Application, Service: job.Service, RevisionId: job.Template.RevisionID, Image: job.Template.Image, Kind: job.Kind, NodeId: job.NodeID, RetryOf: job.RetryOf, CancelRequested: job.CancelRequested, Status: jobRunStatusMessage(job.Status)}
}
func jobError(err error) error {
	code := connect.CodeInternal
	message := "job operation failed"
	switch {
	case errors.Is(err, store.ErrJobNotFound), errors.Is(err, store.ErrApplicationNotFound):
		code, message = connect.CodeNotFound, err.Error()
	case errors.Is(err, store.ErrIdempotencyConflict):
		code, message = connect.CodeAlreadyExists, err.Error()
	case errors.Is(err, store.ErrGenerationConflict):
		code, message = connect.CodeAborted, "application changed; refresh its revision before starting or retrying a job"
	case errors.Is(err, store.ErrJobLimit):
		code, message = connect.CodeResourceExhausted, err.Error()
	case errors.Is(err, store.ErrJobActive), errors.Is(err, errJobPlacement):
		code, message = connect.CodeFailedPrecondition, err.Error()
	default:
		var ce *connect.Error
		if errors.As(err, &ce) {
			return err
		}
	}
	return connect.NewError(code, errors.New(message))
}

// retryNode keeps a retried scheduled-job run on the node that ran it, next
// to any volume it reads. Service jobs are placed again.
func retryNode(source model.Job) string {
	if strings.HasPrefix(source.Service, cronRunServicePrefix) {
		return source.NodeID
	}
	return ""
}

// Called under the reconciler mutex, including for hook creation. A non-empty
// node pins the run to that node.
func (r *Reconciler) createOneOff(ctx context.Context, application, service, revision string, command, args []string, timeout uint32, kind, retry, key, hash string, frozen *model.CronJobDesired, node string) (model.Job, error) {
	if job, found, err := r.state.ReplayJob(ctx, key, hash); err != nil || found {
		return job, err
	}
	app, err := r.state.Application(ctx, application)
	if err != nil {
		return model.Job{}, err
	}
	if app.DeletedAt != nil {
		return model.Job{}, store.ErrApplicationNotFound
	}
	if revision == "" {
		revision = app.RevisionID
	}
	if revision != app.RevisionID {
		return model.Job{}, store.ErrGenerationConflict
	}
	var manifest config.Manifest
	if err := json.Unmarshal(app.CanonicalManifest, &manifest); err != nil {
		return model.Job{}, err
	}
	var source config.Service
	var constraints map[string]string
	if cronJob, isCron := strings.CutPrefix(service, cronRunServicePrefix); isCron {
		scheduled, ok := manifest.CronJobs[cronJob]
		if !ok || frozen == nil {
			return model.Job{}, connect.NewError(connect.CodeInvalidArgument, errors.New("cron job does not exist in this revision"))
		}
		constraints = scheduled.Constraints
	} else {
		var ok bool
		if source, ok = manifest.Services[service]; !ok {
			return model.Job{}, connect.NewError(connect.CodeInvalidArgument, errors.New("service does not exist in this revision"))
		}
		constraints = source.Constraints
	}
	if frozen == nil && (len(command) == 0 || len(command) > 256 || len(args) > 256) {
		return model.Job{}, connect.NewError(connect.CodeInvalidArgument, errors.New("provide 1-256 command entries and at most 256 arguments"))
	}
	if timeout == 0 {
		timeout = 900
	}
	if timeout < 10 || timeout > 86400 {
		return model.Job{}, connect.NewError(connect.CodeInvalidArgument, errors.New("timeout must be 10-86400 seconds"))
	}
	now := r.now().UTC()
	id := jobID(key)
	template := model.CronJobDesired{Application: application, Job: "run-" + strings.TrimPrefix(id, "job_"), ApplicationGeneration: app.Generation, RevisionID: revision, RunID: id, RunAt: &now, Image: source.Image, Command: command, Args: args, Environment: cloneEnvironment(source.Env), DependsOn: append([]string(nil), source.DependsOn...), StartDeadlineSeconds: 60, TimeoutSeconds: timeout, NetworkGeneration: r.cluster.NetworkGeneration, PrivateNetwork: manifest.AllowPrivateNetwork}
	if frozen != nil {
		template = *frozen
		template.RunID = id
		template.Job = "run-" + strings.TrimPrefix(id, "job_")
		template.RunAt = &now
		template.CancelRequested = false
		template.Schedule = ""
	} else {
		template.CPUMillis, err = parseCPUMillis(source.Resources.CPU)
		if err != nil {
			return model.Job{}, err
		}
		template.MemoryBytes, err = parseMemoryBytes(source.Resources.Memory)
		if err != nil {
			return model.Job{}, err
		}
		// Only the selected service's secrets are needed by this run.
		manifest.Services = map[string]config.Service{service: source}
		manifest.CronJobs = nil
		app.CanonicalManifest, _ = config.CanonicalJSON(&manifest)
		resolved, blocked, err := r.resolveApplicationIntent(ctx, app)
		if err != nil {
			return model.Job{}, err
		}
		if blocked != nil {
			return model.Job{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("the service's referenced secrets are unavailable"))
		}
		defer resolved.wipe()
		template.SecretEnvironment, template.SecretFiles = desiredSecretBindings(source.SecretEnv, source.SecretFiles, resolved)
		template.RegistryCredential = desiredRegistryCredential(source.Image, resolved)
	}
	cluster, err := r.state.Cluster(ctx)
	if err != nil {
		return model.Job{}, err
	}
	template.NetworkGeneration = cluster.NetworkGeneration
	template.SpecDigest, err = model.CronJobSpecDigest(template)
	if err != nil {
		return model.Job{}, err
	}
	if err := orchruntime.ValidateCronJob(template); err != nil {
		return model.Job{}, connect.NewError(connect.CodeInvalidArgument, errors.New("job command, image, resources or secret bindings are invalid"))
	}
	nodes, err := r.state.Nodes(ctx)
	if err != nil {
		return model.Job{}, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Ordinal < nodes[j].Ordinal })
	pinned := node
	for _, node := range nodes {
		if pinned != "" && node.ID != pinned {
			continue
		}
		if node.Lifecycle != model.NodeLifecycleActive || node.LastSeenAt == nil || now.Sub(*node.LastSeenAt) >= distributedLossGrace || nodeBlocker(node) != nil || node.NetworkState != model.NetworkReady || node.DNSState != model.SubsystemReady || node.NetworkGeneration != cluster.NetworkGeneration {
			continue
		}
		if r.sessions != nil && !r.sessions.Connected(node.ID) {
			continue
		}
		network, err := model.NodeNetwork(cluster.WorkloadCIDR, node.Ordinal, cluster.NetworkGeneration)
		if err != nil {
			return model.Job{}, err
		}
		if node.NodeSubnet != network.NodeSubnet || (len(nodes) > 1 && !validMeshIdentity(node, network)) {
			continue
		}
		labels := cloneEnvironment(node.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		labels["orch.os"] = node.OperatingSystem
		labels["orch.arch"] = node.Architecture
		labels["orch.node"] = node.Name
		matches := true
		for k, v := range constraints {
			if labels[k] != v {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		cpu, memory := node.AllocatableCPUMillis, node.AllocatableMemoryBytes
		if cpu == 0 {
			cpu = node.TotalCPUMillis
		}
		if memory == 0 {
			memory = node.TotalMemoryBytes
		}
		if cpu == 0 || memory == 0 {
			continue
		}
		job := model.Job{ID: id, NodeID: node.ID, Service: service, Kind: kind, RetryOf: retry, Template: template, Status: model.JobRunStatus{ID: id, Application: application, Job: template.Job, SpecDigest: template.SpecDigest, ScheduledAt: now, State: model.JobRunWaiting, Reason: "assignment_pending", Message: "waiting for the owning agent", UpdatedAt: now}}
		accepted, err := r.state.CreateJob(ctx, job, key, hash, cpu, memory)
		if errors.Is(err, store.ErrJobLimit) {
			continue
		}
		return accepted, err
	}
	return model.Job{}, errJobPlacement
}

func (r *Reconciler) hookGate(ctx context.Context, app model.Application, manifest config.Manifest, now time.Time) (*model.ApplicationExecution, error) {
	if manifest.Deploy.PreDeploy == nil {
		return nil, nil
	}
	job, err := r.state.HookJob(ctx, app.Name, app.RevisionID)
	if errors.Is(err, store.ErrJobNotFound) {
		hook := manifest.Deploy.PreDeploy
		timeout, _ := time.ParseDuration(hook.Timeout)
		key := "hook:" + app.RevisionID
		hash := sha256.Sum256([]byte(key))
		job, err = r.createOneOff(ctx, app.Name, hook.Service, app.RevisionID, hook.Command, hook.Args, uint32(timeout/time.Second), "pre_deploy", "", key, hex.EncodeToString(hash[:]), nil, "")
		if err != nil {
			state := executionState(model.ExecutionBlocked, app.Generation, "pre_deploy_waiting", "pre-deploy job is waiting for compatible capacity and available secrets", now)
			state.ActiveGeneration, state.ActiveRevisionID = app.Execution.ActiveGeneration, app.Execution.ActiveRevisionID
			return &state, nil
		}
	} else if err != nil {
		return nil, err
	}
	if job.Status.State == model.JobRunSucceeded {
		return nil, nil
	}
	status, reason, message := model.ExecutionBlocked, "pre_deploy_running", "waiting for pre-deploy job "+job.ID
	if job.Status.FinishedAt != nil {
		status, reason, message = model.ExecutionFailed, "pre_deploy_failed", "pre-deploy job "+job.ID+" failed; inspect its logs and explicitly retry"
	}
	state := executionState(status, app.Generation, reason, message, now)
	state.ActiveGeneration, state.ActiveRevisionID = app.Execution.ActiveGeneration, app.Execution.ActiveRevisionID
	return &state, nil
}
func (r *Reconciler) appendOneOffAssignments(ctx context.Context, byNode map[string][]model.CronJobDesired) error {
	jobs, err := r.state.Jobs(ctx, "", "", 100, true)
	if err != nil {
		return err
	}
	for node, items := range byNode {
		filtered := items[:0]
		for _, item := range items {
			if item.RunID == "" {
				filtered = append(filtered, item)
			}
		}
		byNode[node] = filtered
	}
	for _, job := range jobs {
		byNode[job.NodeID] = append(byNode[job.NodeID], job.Template)
	}
	return nil
}
