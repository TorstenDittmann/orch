package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type deploymentProvider struct {
	*fakeProvider
	current     *orchv1alpha1.Application
	history     []*orchv1alpha1.Deployment
	rollback    *orchv1alpha1.RollbackApplicationRequest
	rollbackErr error
}

func (p *deploymentProvider) GetApplication(context.Context, string) (*orchv1alpha1.Application, error) {
	return p.current, p.err
}
func (p *deploymentProvider) ListDeployments(context.Context, string, uint64, uint32) (*orchv1alpha1.ListDeploymentsResponse, error) {
	return &orchv1alpha1.ListDeploymentsResponse{Deployments: p.history, NextBeforeGeneration: 1}, p.err
}
func (p *deploymentProvider) CompareDeployments(_ context.Context, app, from, to string) (*orchv1alpha1.CompareDeploymentsResponse, error) {
	return &orchv1alpha1.CompareDeploymentsResponse{From: &orchv1alpha1.Deployment{Application: app, RevisionId: from}, To: &orchv1alpha1.Deployment{Application: app, RevisionId: to}, Changes: []*orchv1alpha1.DeploymentChange{{Kind: "modify", Path: "services.web.image", Before: "old-image", After: "new-image"}}}, p.err
}
func (p *deploymentProvider) RollbackApplication(_ context.Context, r *orchv1alpha1.RollbackApplicationRequest) (*orchv1alpha1.ApplyApplicationResponse, error) {
	p.rollback = r
	return &orchv1alpha1.ApplyApplicationResponse{Name: r.GetApplication(), Generation: r.GetExpectedGeneration() + 1, RevisionId: "rev-three"}, p.rollbackErr
}

func deploymentFixture() (*Model, *deploymentProvider) {
	now := timestamppb.Now()
	p := &deploymentProvider{fakeProvider: fixtureProvider()}
	p.current = &orchv1alpha1.Application{Name: "web", Generation: 2, RevisionId: "rev-two", ActiveGeneration: 1, ActiveRevisionId: "rev-one", ExecutionStatus: orchv1alpha1.ExecutionStatus_EXECUTION_STATUS_STARTING, Allocations: []*orchv1alpha1.AllocationStatus{{Application: "web", Service: "web", Replica: 1, NodeName: "node-1", Phase: orchv1alpha1.AllocationPhase_ALLOCATION_PHASE_STARTING}}}
	p.history = []*orchv1alpha1.Deployment{{Application: "web", Generation: 2, RevisionId: "rev-two", Outcome: "IN_PROGRESS", CreatedAt: now}, {Application: "web", Generation: 1, RevisionId: "rev-one", Outcome: "SUCCEEDED", CreatedAt: now, Images: []*orchv1alpha1.DeploymentImage{{Kind: "service", Name: "web", Image: "docker.io/library/busybox@sha256:" + strings.Repeat("a", 64)}}}}
	m := New(context.Background(), p, "production", "ssh", time.Minute)
	m.snapshot = fixtureSnapshot(p.fakeProvider)
	cmd := m.openDeployments("web")
	m.receiveDeployments(cmd().(deploymentsMsg))
	return m, p
}

func TestDeploymentNavigationComparisonAndStaleResponses(t *testing.T) {
	m, _ := deploymentFixture()
	if m.view != ViewDeployments || len(m.deploymentHistory) != 2 {
		t.Fatal("history not opened")
	}
	m.handleDeploymentKey("down")
	_, cmd := m.handleDeploymentKey("enter")
	msg := cmd().(deploymentDiffMsg)
	m.receiveDeploymentDiff(msg)
	if m.deploymentDiff == nil || m.deploymentDiff.GetFrom().GetRevisionId() != "rev-one" {
		t.Fatal("wrong selected revision compared")
	}
	m.deploymentDetail = false
	m.handleDeploymentKey("up")
	m.receiveDeploymentDiff(msg)
	if m.deploymentDiff != nil {
		t.Fatal("late diff overwrote another selection")
	}
	m.deploymentSelection = 1
	generation := m.deploymentGeneration
	m.receiveDeployments(deploymentsMsg{generation: generation - 1, app: "web", err: errors.New("old failure")})
	if m.deploymentError != "" {
		t.Fatal("late history error applied")
	}
	current := m.deploymentCurrent
	m.receiveDeployments(deploymentsMsg{generation: generation, app: "web", err: errors.New("disconnected")})
	if m.deploymentCurrent != current || m.deploymentError == "" {
		t.Fatal("failed refresh discarded last good snapshot")
	}
	m.setView(ViewApplications)
	m.receiveDeployments(deploymentsMsg{generation: generation, app: "web", current: &orchv1alpha1.Application{Name: "foreign"}})
	if m.deploymentCurrent != current {
		t.Fatal("canceled workspace accepted response")
	}
}

func TestRollbackConfirmationGuardsGenerationAndRetriesExactRequest(t *testing.T) {
	m, p := deploymentFixture()
	m.deploymentSelection = 1
	m.prepareRollback()
	if m.confirm == nil || !m.rollbackConfirmationCurrent() {
		t.Fatal("missing rollback confirmation")
	}
	p.applications[0].Generation = 3
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd != nil || m.confirm != nil || p.rollback != nil {
		t.Fatal("stale confirmation submitted")
	}
	p.applications[0].Generation = 2
	m.prepareRollback()
	request := m.confirm.rollback
	p.rollbackErr = errors.New("reply lost")
	_, cmd = m.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m.receiveRollback(cmd().(rollbackMsg))
	if m.confirm == nil || !m.confirm.rollbackRetry || m.confirm.rollback != request {
		t.Fatal("uncertain retry lost request identity")
	}
	p.applications[0].Generation = 3
	_, cmd = m.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("exact uncertain retry blocked by refreshed generation")
	}
	p.rollbackErr = nil
	m.receiveRollback(cmd().(rollbackMsg))
	if p.rollback != request || m.confirm != nil {
		t.Fatal("retry changed request or left confirmation open")
	}
}

func TestDeploymentResponsiveLayoutsAndConfirmation(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m, _ := deploymentFixture()
	m.deploymentSelection = 1
	for _, size := range [][2]int{{60, 18}, {80, 24}, {120, 40}} {
		m.width, m.height = size[0], size[1]
		for _, detail := range []bool{false, true} {
			m.deploymentDetail = detail
			view := m.View().Content
			if !strings.Contains(view, "production") {
				t.Fatal("target missing")
			}
			if len(strings.Split(view, "\n")) > size[1] {
				t.Fatalf("%dx%d detail=%t height overflow", size[0], size[1], detail)
			}
			for _, line := range strings.Split(view, "\n") {
				if lipgloss.Width(line) > size[0] {
					t.Fatalf("%dx%d width overflow: %q", size[0], size[1], line)
				}
			}
		}
		m.prepareRollback()
		view := m.View().Content
		if !strings.Contains(view, "y confirm") || !strings.Contains(view, "Replace generation: 2") {
			t.Fatalf("confirmation inaccessible at %dx%d: %s", size[0], size[1], view)
		}
		if len(strings.Split(view, "\n")) > size[1] {
			t.Fatalf("confirmation overflows %dx%d", size[0], size[1])
		}
		m.confirm = nil
	}
}

func TestDeploymentImageReferenceShowsRequestedAndResolved(t *testing.T) {
	image := &orchv1alpha1.DeploymentImage{RequestedImage: "example/web:latest", Image: "docker.io/example/web@sha256:" + strings.Repeat("a", 64)}
	if got := deploymentImageReference(image); got != image.RequestedImage+" -> "+image.Image {
		t.Fatalf("image display = %s", got)
	}
	image.RequestedImage = ""
	if got := deploymentImageReference(image); got != image.Image {
		t.Fatalf("legacy image display = %s", got)
	}
}
