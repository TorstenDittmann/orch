package control

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
)

func TestNotifierSendsWantedEventsOnce(t *testing.T) {
	state, cluster := reconcilerStore(t)
	box, err := security.NewSecretBox(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), principalKey{}, authenticatedPrincipal{certificate: &x509.Certificate{}})
	var mu sync.Mutex
	received := map[string][]string{}
	failures := 1
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/hook" && failures > 0 {
			failures--
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		received[r.URL.Path] = append(received[r.URL.Path], string(body))
	}))
	defer endpoint.Close()

	n := newNotifier(state, box, "prod")
	n.retries = []time.Duration{time.Millisecond}
	if err := n.deliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	s := newService(state, cluster, nil, nil, box, nil, time.Now)
	s.imageResolver = imageResolverFunc(func(_ context.Context, image, _ string, _ []byte) (string, error) {
		return "docker.io/library/busybox@sha256:" + strings.Repeat("1", 64), nil
	})
	for name, path := range map[string]string{"slack-url": "/slack", "hook-url": "/hook"} {
		if _, err := state.SetApplicationSecret(ctx, box, "hello", name, "secv_"+strings.ReplaceAll(name, "-", ""), []byte(endpoint.URL+path), "idempotency-"+name, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	manifest := newTestManifest("hello", map[string]config.Service{"worker": {Image: "busybox:latest"}})
	manifest.Notifications = []config.Notification{
		{Kind: config.NotificationSlack, URLSecret: "slack-url", On: config.DefaultNotificationEvents},
		{Kind: config.NotificationWebhook, URLSecret: "hook-url", On: []string{config.NotifyDeploySucceeded}},
	}
	applied, err := s.ApplyApplication(ctx, connect.NewRequest(&orchv1alpha1.ApplyApplicationRequest{CanonicalManifestJson: mustCanonicalConfig(t, manifest), IdempotencyKey: "notify-deploy-one"}))
	if err != nil {
		t.Fatal(err)
	}
	execution := model.ApplicationExecution{Status: model.ExecutionHealthy, ObservedGeneration: applied.Msg.GetGeneration(),
		ActiveGeneration: applied.Msg.GetGeneration(), ActiveRevisionID: applied.Msg.GetRevisionId(), Reason: "ready", UpdatedAt: time.Now().UTC()}
	if err := state.UpdateApplicationExecutions(ctx, map[string]model.ApplicationExecution{"hello": execution}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := n.deliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received["/slack"]) != 0 {
		t.Fatalf("slack was sent an event it did not ask for: %v", received["/slack"])
	}
	if len(received["/hook"]) != 1 {
		t.Fatalf("webhook deliveries = %v", received["/hook"])
	}
	var body struct {
		Event, Cluster, Application, Text string
		Data                              model.DeploymentFinished
	}
	if err := json.Unmarshal([]byte(received["/hook"][0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.Event != config.NotifyDeploySucceeded || body.Cluster != "prod" || body.Application != "hello" ||
		body.Text != "[prod] hello: deploy succeeded" || body.Data.Outcome != "SUCCEEDED" {
		t.Fatalf("webhook body = %+v", body)
	}
}

func TestNotificationBodies(t *testing.T) {
	n := newNotifier(nil, nil, "prod")
	text := "[prod] shop: deploy failed: " + strings.Repeat("x", 3000)
	for kind, field := range map[string]string{config.NotificationSlack: "text", config.NotificationDiscord: "content"} {
		body, err := n.notificationBody(kind, notice{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if value, _ := decoded[field].(string); !strings.HasPrefix(value, "[prod] shop: deploy failed") || len(value) > 2000 {
			t.Fatalf("%s body = %s", kind, body)
		}
	}
	if got := describeSource(model.DeploymentSource{Ref: "refs/heads/main", Commit: "0123456789abcdef", Actor: "torsten"}); got != " (main@0123456 by torsten)" {
		t.Fatalf("source = %q", got)
	}
}

func TestNotifierDescribesFailures(t *testing.T) {
	state, _ := reconcilerStore(t)
	n := newNotifier(state, nil, "prod")
	exitCode := int32(3)
	payload := func(value any) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, test := range []struct {
		event       model.Event
		wantedEvent string
		text        string
	}{
		{model.Event{Kind: model.EventDeploymentFinished, Payload: payload(model.DeploymentFinished{Application: "shop", RevisionID: "rev_missing", Outcome: "FAILED", Message: "health deadline exceeded"})},
			config.NotifyDeployFailed, "[prod] shop: deploy failed: health deadline exceeded"},
		{model.Event{Kind: model.EventDeploymentFinished, Payload: payload(model.DeploymentFinished{Application: "shop", RevisionID: "rev_missing", Outcome: "ROLLED_BACK", Message: "web crashed"})},
			config.NotifyDeployRolledBack, "[prod] shop: deploy rolled back: web crashed"},
		{model.Event{Kind: model.EventCronRunFailed, Payload: payload(model.CronRunFailed{Application: "shop", Job: "nightly", State: model.JobRunFailed, ExitCode: &exitCode})},
			config.NotifyCronFailed, "[prod] shop: cron job nightly failed with exit code 3"},
		{model.Event{Kind: model.EventCronRunFailed, Payload: payload(model.CronRunFailed{Application: "shop", Job: "nightly", State: model.JobRunTimedOut})},
			config.NotifyCronFailed, "[prod] shop: cron job nightly timed out"},
		{model.Event{Kind: model.EventVolumeBackupFailed, Payload: payload(model.VolumeBackupFailed{Application: "shop", Volume: "data", Message: "disk full"})},
			config.NotifyBackupFailed, "[prod] shop: backup of volume data failed: disk full"},
	} {
		got, ok := n.describe(context.Background(), test.event)
		if !ok || got.Event != test.wantedEvent || got.Application != "shop" || got.Text != test.text {
			t.Fatalf("describe(%s) = %+v, %v; want %s %q", test.event.Kind, got, ok, test.wantedEvent, test.text)
		}
		defaults := config.Notification{On: config.DefaultNotificationEvents}
		if !defaults.Wants(got.Event) {
			t.Fatalf("default notifications skip %s", got.Event)
		}
		if (config.Notification{On: []string{config.NotifyDeploySucceeded}}).Wants(got.Event) {
			t.Fatalf("a deploy.succeeded notification wants %s", got.Event)
		}
	}
	if _, ok := n.describe(context.Background(), model.Event{Kind: model.EventDeploymentFinished, Payload: payload(model.DeploymentFinished{Application: "shop", Outcome: "SUPERSEDED"})}); ok {
		t.Fatal("a superseded deploy was described")
	}
}
