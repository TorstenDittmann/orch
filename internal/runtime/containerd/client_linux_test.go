//go:build linux

package containerd

import (
	"testing"
	"time"
)

func TestTaskStartedAtPrefersRecordedTaskStart(t *testing.T) {
	created := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	started := created.Add(time.Hour)
	labels := map[string]string{labelStartedAt: started.Format(time.RFC3339Nano)}
	if got := taskStartedAt(labels, created); !got.Equal(started) {
		t.Fatalf("taskStartedAt() = %v, want %v", got, started)
	}
	for _, value := range []string{"", "not-a-time"} {
		if got := taskStartedAt(map[string]string{labelStartedAt: value}, created); !got.Equal(created) {
			t.Fatalf("taskStartedAt(%q) = %v, want creation time %v", value, got, created)
		}
	}
}
