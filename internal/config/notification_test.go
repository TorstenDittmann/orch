package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLoadNotifications(t *testing.T) {
	manifest := loadManifest(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"
name = "shop"
services { ["web"] { image = "example/web:1" } }
notifications {
  new Slack { url = "slack-webhook" }
  new Webhook { url = "ops-hook"; on { "replica.unhealthy"; "deploy.succeeded" } }
}
`)
	want := []Notification{
		{Kind: NotificationSlack, URLSecret: "slack-webhook", On: []string{NotifyBackupFailed, NotifyCronFailed, NotifyDeployFailed, NotifyDeployRolledBack}},
		{Kind: NotificationWebhook, URLSecret: "ops-hook", On: []string{NotifyDeploySucceeded, NotifyReplicaUnhealthy}},
	}
	if !reflect.DeepEqual(manifest.Notifications, want) {
		t.Fatalf("notifications = %#v", manifest.Notifications)
	}
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCanonicalJSON(canonical); err != nil {
		t.Fatalf("ParseCanonicalJSON() error = %v", err)
	}
	plain, err := CanonicalJSON(loadManifest(t, `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"
name = "shop"
services { ["web"] { image = "example/web:1" } }
`))
	if err != nil || strings.Contains(string(plain), "notifications") {
		t.Fatalf("a manifest without notifications changed: %s %v", plain, err)
	}
}

func TestValidateRejectsInvalidNotifications(t *testing.T) {
	for name, test := range map[string]struct {
		notification Notification
		path         string
	}{
		"kind":       {Notification{Kind: "email", URLSecret: "hook", On: []string{NotifyDeployFailed}}, "notifications[0].kind"},
		"secret":     {Notification{Kind: NotificationSlack, URLSecret: "Not A Label", On: []string{NotifyDeployFailed}}, "notifications[0].url_secret"},
		"no events":  {Notification{Kind: NotificationSlack, URLSecret: "hook"}, "notifications[0].on"},
		"bad event":  {Notification{Kind: NotificationSlack, URLSecret: "hook", On: []string{"node.lost"}}, "notifications[0].on"},
		"duplicated": {Notification{Kind: NotificationSlack, URLSecret: "hook", On: []string{NotifyDeployFailed, NotifyDeployFailed}}, "notifications[0].on"},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := loadManifest(t, validManifest)
			manifest.Notifications = []Notification{test.notification}
			var validation *ValidationError
			if !errors.As(Validate(manifest), &validation) || !hasProblem(validation, test.path) {
				t.Fatalf("Validate() = %v", validation)
			}
		})
	}
}
