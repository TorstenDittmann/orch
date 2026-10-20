package config

import (
	"fmt"
	"sort"
)

// Notification kinds and the events they can be sent.
const (
	NotificationSlack   = "slack"
	NotificationDiscord = "discord"
	NotificationWebhook = "webhook"

	NotifyDeploySucceeded  = "deploy.succeeded"
	NotifyDeployFailed     = "deploy.failed"
	NotifyDeployRolledBack = "deploy.rolled_back"
	NotifyBackupFailed     = "backup.failed"
	NotifyCronFailed       = "cron.failed"
	NotifyReplicaUnhealthy = "replica.unhealthy"

	maxNotifications = 20
)

// DefaultNotificationEvents are sent when a notification names none.
var DefaultNotificationEvents = []string{NotifyBackupFailed, NotifyCronFailed, NotifyDeployFailed, NotifyDeployRolledBack}

var notificationEvents = map[string]bool{
	NotifyDeploySucceeded: true, NotifyDeployFailed: true, NotifyDeployRolledBack: true,
	NotifyBackupFailed: true, NotifyCronFailed: true, NotifyReplicaUnhealthy: true,
}

// Notification sends an application's events to a webhook whose URL is kept
// in an application secret.
type Notification struct {
	Kind      string   `json:"kind"`
	URLSecret string   `json:"url_secret"`
	On        []string `json:"on"`
}

// Wants reports whether the notification is sent event.
func (n Notification) Wants(event string) bool {
	for _, wanted := range n.On {
		if wanted == event {
			return true
		}
	}
	return false
}

func validateNotifications(notifications []Notification, add func(string, string)) {
	if len(notifications) > maxNotifications {
		add("notifications", fmt.Sprintf("must not define more than %d notifications", maxNotifications))
	}
	for index, notification := range notifications {
		base := fmt.Sprintf("notifications[%d]", index)
		switch notification.Kind {
		case NotificationSlack, NotificationDiscord, NotificationWebhook:
		default:
			add(base+".kind", "must be slack, discord or webhook")
		}
		validateDNSLabel(base+".url_secret", notification.URLSecret, add)
		if len(notification.On) == 0 {
			add(base+".on", "must name at least one event")
		}
		seen := make(map[string]bool, len(notification.On))
		for _, event := range notification.On {
			if !notificationEvents[event] {
				add(base+".on", fmt.Sprintf("%q is not a notification event", event))
			} else if seen[event] {
				add(base+".on", fmt.Sprintf("%q is listed twice", event))
			}
			seen[event] = true
		}
	}
}

func normalizeNotifications(manifest *Manifest) {
	for index := range manifest.Notifications {
		sort.Strings(manifest.Notifications[index].On)
	}
	if len(manifest.Notifications) == 0 {
		manifest.Notifications = nil
	}
}
