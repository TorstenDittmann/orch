package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
	clusterctl "github.com/TorstenDittmann/orch/internal/cluster"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"google.golang.org/protobuf/proto"
)

func TestDeploymentAPIHistoryComparisonRollbackAndUnknownFields(t *testing.T) {
	_, dir, path, _ := bootstrapController(t)
	ctx := context.Background()
	server, err := Start(ctx, Options{DataDir: dir, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	endpoint := "https://" + server.Addr().String()
	rewriteContextServer(t, path, endpoint)
	remote, err := controlclient.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.CloseIdleConnections)
	image1 := "docker.io/library/busybox@sha256:" + strings.Repeat("1", 64)
	image2 := "docker.io/library/busybox@sha256:" + strings.Repeat("2", 64)
	firstManifest := newTestManifest("hello", map[string]config.Service{"web": {Image: image1, Env: map[string]string{"TOKEN": "private-old"}, Args: []string{"private-command-old"}}})
	first, err := remote.ApplyApplication(ctx, mustCanonicalConfig(t, firstManifest), "idempotency-key-one")
	if err != nil {
		t.Fatal(err)
	}
	secondManifest := newTestManifest("hello", map[string]config.Service{"web": {Image: image2, Env: map[string]string{"TOKEN": "private-new"}, Args: []string{"private-command-new"}}, "extra": {Image: image1, Env: map[string]string{"TOKEN": "private-added"}}})
	web := secondManifest.Services["web"]
	web.Capabilities = []string{"CAP_CHOWN"}
	secondManifest.Services["web"] = web
	second, err := remote.ApplyApplication(ctx, mustCanonicalConfig(t, secondManifest), "idempotency-key-two")
	if err != nil {
		t.Fatal(err)
	}
	history, err := remote.ListDeployments(ctx, "hello", 0, 1)
	if err != nil || len(history.GetDeployments()) != 1 || history.GetNextBeforeGeneration() != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	diff, err := remote.CompareDeployments(ctx, "hello", first.GetRevisionId(), "")
	if err != nil {
		t.Fatal(err)
	}
	if diff.GetFrom().GetRevisionId() != first.GetRevisionId() || diff.GetTo().GetRevisionId() != second.GetRevisionId() {
		t.Fatal("comparison identities differ")
	}
	encoded, _ := json.Marshal(diff)
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("comparison leaked sensitive fields: %s", encoded)
	}
	foundImage, foundEnv, foundCapabilities := false, false, false
	for _, c := range diff.GetChanges() {
		if c.GetPath() == "services.web.image" && !c.GetRedacted() && strings.Contains(c.GetAfter(), image2) {
			foundImage = true
		}
		if c.GetPath() == "services.web.env.TOKEN" && c.GetRedacted() {
			foundEnv = true
		}
		if c.GetPath() == "services.web.capabilities" && !c.GetRedacted() && strings.Contains(c.GetAfter(), "CAP_CHOWN") {
			foundCapabilities = true
		}
	}
	if !foundImage || !foundEnv || !foundCapabilities {
		t.Fatalf("missing image/env comparison: %s", encoded)
	}
	request := &orchv1alpha1.RollbackApplicationRequest{Application: "hello", RevisionId: first.GetRevisionId(), ExpectedGeneration: 2, IdempotencyKey: "idempotency-rollback"}
	rolled, err := remote.RollbackApplication(ctx, request)
	if err != nil || rolled.GetGeneration() != 3 || rolled.GetDigest() != first.GetDigest() {
		t.Fatalf("rollback: %+v %v", rolled, err)
	}
	replay, err := remote.RollbackApplication(ctx, request)
	if err != nil || !proto.Equal(rolled, replay) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	current, err := remote.GetApplication(ctx, "hello")
	if err != nil || current.GetRevisionId() != rolled.GetRevisionId() || current.GetCanonicalManifestJson() != string(mustCanonicalConfig(t, firstManifest)) {
		t.Fatalf("stored rollback: %+v %v", current, err)
	}
	d, err := remote.GetDeployment(ctx, "hello", rolled.GetRevisionId())
	if err != nil || d.GetRollbackFromRevisionId() != first.GetRevisionId() {
		t.Fatalf("provenance: %+v %v", d, err)
	}
	stale := proto.Clone(request).(*orchv1alpha1.RollbackApplicationRequest)
	stale.IdempotencyKey = "idempotency-stale"
	if _, err := remote.RollbackApplication(ctx, stale); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale rollback: %v", err)
	}
	conflict := proto.Clone(request).(*orchv1alpha1.RollbackApplicationRequest)
	conflict.ExpectedGeneration = 3
	if _, err := remote.RollbackApplication(ctx, conflict); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("conflicting replay: %v", err)
	}
	if _, err := remote.GetDeployment(ctx, "other", first.GetRevisionId()); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("foreign read: %v", err)
	}
	foreign := proto.Clone(request).(*orchv1alpha1.RollbackApplicationRequest)
	foreign.Application = "other"
	if _, err := remote.RollbackApplication(ctx, foreign); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("foreign rollback: %v", err)
	}
	if _, err := remote.ListDeployments(ctx, "hello", 0, 51); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("history limit: %v", err)
	}
	credentials, err := clusterctl.LoadAdministratorCredentials(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	generated := generatedClient(tlsHTTPClient(credentials.RootCAs, credentials.Certificate), endpoint)
	unknown := &orchv1alpha1.ListDeploymentsRequest{Application: "hello"}
	unknown.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if _, err := generated.ListDeployments(ctx, connect.NewRequest(unknown)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown fields: %v", err)
	}
}

func TestDeploymentMethodsRequireAdministratorBeforeReadingState(t *testing.T) {
	ctx := context.Background()
	s := &service{}
	checks := []func() error{
		func() error {
			_, err := s.ListDeployments(ctx, connect.NewRequest(&orchv1alpha1.ListDeploymentsRequest{Application: "hello"}))
			return err
		},
		func() error {
			_, err := s.GetDeployment(ctx, connect.NewRequest(&orchv1alpha1.GetDeploymentRequest{Application: "hello", RevisionId: "rev-one"}))
			return err
		},
		func() error {
			_, err := s.CompareDeployments(ctx, connect.NewRequest(&orchv1alpha1.CompareDeploymentsRequest{Application: "hello", FromRevisionId: "rev-one"}))
			return err
		},
		func() error {
			_, err := s.RollbackApplication(ctx, connect.NewRequest(&orchv1alpha1.RollbackApplicationRequest{Application: "hello", RevisionId: "rev-one", ExpectedGeneration: 1, IdempotencyKey: "idempotency-key-one"}))
			return err
		},
	}
	for i, check := range checks {
		if err := check(); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("method %d: %v", i, err)
		}
	}
}

func TestManualRollbackReconcilesAndBecomesReadyAsANewRevision(t *testing.T) {
	ctx := context.Background()
	state, cluster := reconcilerStore(t)
	enrollCompatibleNode(t, state)
	now := reconcilerTestTime.Add(100 * time.Second)
	reconciler := NewReconciler(state, cluster, func() time.Time { return now }, nil)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	preflight, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	freezeReconcilerNetwork(t, state, preflight)
	first := runtimeManifest(executableImage)
	applyManifest(t, state, first, "revision-one", "application-key-one", now)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err := state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	recordReadyAssignment(t, state, assignment, 2, now)
	now = now.Add(time.Second)
	second := runtimeManifest(strings.Replace(executableImage, strings.Repeat("a", 64), strings.Repeat("b", 64), 1))
	applyManifest(t, state, second, "revision-two", "application-key-two", now)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err = state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	recordReadyAssignment(t, state, assignment, 3, now)
	source, err := state.Deployment(ctx, "hello", "revision-one")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	result, err := state.ApplyApplication(ctx, model.ApplyApplication{Name: "hello", RevisionID: "revision-three", Digest: source.Digest, CanonicalManifest: source.CanonicalManifest, IdempotencyKey: "application-rollback", CreatedAt: now, ExpectedGeneration: 2, RollbackFromRevisionID: source.RevisionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assignment, err = state.CurrentAssignment(ctx, "node-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(assignment.Allocations) != 1 || assignment.Allocations[0].Image != executableImage || assignment.Allocations[0].ApplicationGeneration != 3 || assignment.Allocations[0].RevisionID != result.RevisionID {
		t.Fatalf("rollback assignment: %+v", assignment.Allocations)
	}
	recordReadyAssignment(t, state, assignment, 4, now)
	current, err := state.Application(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := controlclient.DeploymentReady(applicationMessage(current), 3, result.RevisionID)
	if err != nil || !ready {
		t.Fatalf("rollback readiness=%t %v execution=%+v", ready, err, current.Execution)
	}
	d, err := state.Deployment(ctx, "hello", result.RevisionID)
	if err != nil || d.Outcome != "SUCCEEDED" || d.RollbackFromRevisionID != "revision-one" {
		t.Fatalf("rollback result=%+v %v", d, err)
	}
}

func TestDeploymentSourceValidation(t *testing.T) {
	if source, err := deploymentSource(nil); source != nil || err != nil {
		t.Fatalf("nil source: %+v %v", source, err)
	}
	if source, err := deploymentSource(&orchv1alpha1.DeploymentSource{}); source != nil || err != nil {
		t.Fatalf("empty source: %+v %v", source, err)
	}
	valid := &orchv1alpha1.DeploymentSource{Repository: "acme/hello", Commit: "abc", RunUrl: "https://github.com/acme/hello/actions/runs/1"}
	if source, err := deploymentSource(valid); err != nil || source.RunURL != valid.RunUrl {
		t.Fatalf("valid source: %+v %v", source, err)
	}
	for _, invalid := range []*orchv1alpha1.DeploymentSource{
		{RunUrl: "javascript:alert(1)"},
		{RunUrl: "http://github.com/run"},
		{Actor: "bad\nactor"},
		{Commit: strings.Repeat("a", maxSourceFieldBytes+1)},
	} {
		if _, err := deploymentSource(invalid); err == nil {
			t.Fatalf("accepted %+v", invalid)
		}
	}
}
