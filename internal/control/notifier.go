package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TorstenDittmann/orch/internal/buildinfo"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
)

const (
	notifyPollEvery    = 5 * time.Second
	notifyBatch        = 100
	notifyTimeout      = 10 * time.Second
	notifyMaxTextBytes = 1900
)

// notifier sends the events applications ask for in `notifications` to their
// webhooks. It follows the event log with a durable cursor, so each event is
// handled once even across controller restarts; an endpoint that keeps
// failing is skipped after a few attempts rather than holding up the rest.
type notifier struct {
	state   *store.Store
	box     *security.SecretBox
	cluster string
	client  *http.Client
	retries []time.Duration
}

func newNotifier(state *store.Store, box *security.SecretBox, cluster string) *notifier {
	return &notifier{state: state, box: box, cluster: cluster,
		client:  &http.Client{Timeout: notifyTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		retries: []time.Duration{time.Second, 5 * time.Second}}
}

func (n *notifier) run(ctx context.Context) {
	ticker := time.NewTicker(notifyPollEvery)
	defer ticker.Stop()
	for {
		if err := n.deliverPending(ctx); err != nil && ctx.Err() == nil {
			log.Printf("controller: notifications: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverPending handles every event after the cursor.
func (n *notifier) deliverPending(ctx context.Context) error {
	cursor, err := n.state.NotificationCursor(ctx)
	if err != nil {
		return err
	}
	for {
		events, err := n.state.EventsAfter(ctx, cursor, notifyBatch)
		if err != nil || len(events) == 0 {
			return err
		}
		for _, event := range events {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			n.handle(ctx, event)
			cursor = int64(event.Sequence)
			if err := n.state.SetNotificationCursor(ctx, cursor); err != nil {
				return err
			}
		}
	}
}

// notice is one event as notifications describe it.
type notice struct {
	Event       string
	Application string
	Text        string
	Payload     json.RawMessage
	Time        time.Time
}

func (n *notifier) handle(ctx context.Context, event model.Event) {
	notice, ok := n.describe(ctx, event)
	if !ok {
		return
	}
	application, err := n.state.Application(ctx, notice.Application)
	if err != nil {
		return
	}
	var manifest config.Manifest
	if err := json.Unmarshal(application.CanonicalManifest, &manifest); err != nil {
		return
	}
	for _, notification := range manifest.Notifications {
		if !notification.Wants(notice.Event) {
			continue
		}
		if err := n.send(ctx, notice, notification); err != nil && ctx.Err() == nil {
			log.Printf("controller: notify %s via %s (%s): %v", notice.Application, notification.Kind, notification.URLSecret, err)
		}
	}
}

// describe turns an event into a notice, or reports false for events no
// notification is sent for.
func (n *notifier) describe(ctx context.Context, event model.Event) (notice, bool) {
	result := notice{Payload: event.Payload, Time: event.CreatedAt.UTC()}
	switch event.Kind {
	case model.EventDeploymentFinished:
		var payload model.DeploymentFinished
		if json.Unmarshal(event.Payload, &payload) != nil {
			return notice{}, false
		}
		result.Application = payload.Application
		source := ""
		if deployment, err := n.state.Deployment(ctx, payload.Application, payload.RevisionID); err == nil && deployment.Source != nil {
			source = describeSource(*deployment.Source)
		}
		switch payload.Outcome {
		case "SUCCEEDED":
			result.Event, result.Text = config.NotifyDeploySucceeded, "deploy succeeded"+source
		case "FAILED":
			result.Event, result.Text = config.NotifyDeployFailed, "deploy failed"+source+": "+payload.Message
		case "ROLLED_BACK":
			result.Event, result.Text = config.NotifyDeployRolledBack, "deploy rolled back"+source+": "+payload.Message
		default:
			return notice{}, false
		}
	case model.EventVolumeBackupFailed:
		var payload model.VolumeBackupFailed
		if json.Unmarshal(event.Payload, &payload) != nil {
			return notice{}, false
		}
		result.Application, result.Event = payload.Application, config.NotifyBackupFailed
		result.Text = fmt.Sprintf("backup of volume %s failed: %s", payload.Volume, payload.Message)
	case model.EventCronRunFailed:
		var payload model.CronRunFailed
		if json.Unmarshal(event.Payload, &payload) != nil {
			return notice{}, false
		}
		result.Application, result.Event = payload.Application, config.NotifyCronFailed
		outcome := "failed"
		if payload.State == model.JobRunTimedOut {
			outcome = "timed out"
		} else if payload.ExitCode != nil {
			outcome = fmt.Sprintf("failed with exit code %d", *payload.ExitCode)
		}
		result.Text = fmt.Sprintf("cron job %s %s", payload.Job, outcome)
		if payload.Message != "" {
			result.Text += ": " + payload.Message
		}
	case model.EventAllocationUnhealthy:
		var payload struct {
			Application string `json:"application"`
			Service     string `json:"service"`
			Replica     uint32 `json:"replica"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return notice{}, false
		}
		result.Application, result.Event = payload.Application, config.NotifyReplicaUnhealthy
		result.Text = fmt.Sprintf("replica %d of %s is unhealthy", payload.Replica, payload.Service)
	default:
		return notice{}, false
	}
	result.Text = fmt.Sprintf("[%s] %s: %s", n.cluster, result.Application, result.Text)
	return result, result.Application != ""
}

func describeSource(source model.DeploymentSource) string {
	var parts []string
	if source.Ref != "" {
		parts = append(parts, strings.TrimPrefix(source.Ref, "refs/heads/"))
	}
	if source.Commit != "" {
		commit := source.Commit
		if len(commit) > 7 {
			commit = commit[:7]
		}
		parts = append(parts, commit)
	}
	text := strings.Join(parts, "@")
	if source.Actor != "" {
		text = strings.TrimSpace(text + " by " + source.Actor)
	}
	if text == "" {
		return ""
	}
	return " (" + text + ")"
}

// notificationBody renders a notice for one kind of endpoint.
func (n *notifier) notificationBody(kind string, notice notice) ([]byte, error) {
	text := notice.Text
	if len(text) > notifyMaxTextBytes {
		text = strings.ToValidUTF8(text[:notifyMaxTextBytes], "") + "…"
	}
	switch kind {
	case config.NotificationSlack:
		return json.Marshal(map[string]string{"text": text})
	case config.NotificationDiscord:
		return json.Marshal(map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
	default:
		return json.Marshal(struct {
			Event       string          `json:"event"`
			Cluster     string          `json:"cluster"`
			Application string          `json:"application"`
			Text        string          `json:"text"`
			Time        time.Time       `json:"time"`
			Data        json.RawMessage `json:"data"`
		}{notice.Event, n.cluster, notice.Application, notice.Text, notice.Time, notice.Payload})
	}
}

func (n *notifier) send(ctx context.Context, notice notice, notification config.Notification) error {
	secret, err := n.state.ResolveApplicationSecret(ctx, n.box, notice.Application, notification.URLSecret)
	if errors.Is(err, store.ErrSecretNotFound) {
		return fmt.Errorf("secret %s is not set; set it with: orch secret set %s %s --from-stdin", notification.URLSecret, notice.Application, notification.URLSecret)
	}
	if err != nil {
		return fmt.Errorf("read secret %s: %w", notification.URLSecret, err)
	}
	target, err := url.Parse(strings.TrimSpace(string(secret.Value)))
	wipeBytes(secret.Value)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" {
		return fmt.Errorf("secret %s does not hold an http or https URL", notification.URLSecret)
	}
	body, err := n.notificationBody(notification.Kind, notice)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		retry, err := n.post(ctx, target.String(), body)
		if err == nil {
			return nil
		}
		if !retry || attempt >= len(n.retries) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(n.retries[attempt]):
		}
	}
}

// post sends one request and reports whether a failure is worth retrying.
func (n *notifier) post(ctx context.Context, target string, body []byte) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "orch/"+buildinfo.Version)
	response, err := n.client.Do(request)
	if err != nil {
		// The URL is a secret and errors from the client quote it.
		return true, errors.New("the request failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return false, nil
	}
	retry := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	return retry, fmt.Errorf("the endpoint answered %s", response.Status)
}
