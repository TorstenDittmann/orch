package schedule

import (
	"testing"
	"time"
)

func TestParseAndNextUseFiveFieldUTCContract(t *testing.T) {
	got, err := Next("*/2 * * * *", time.Date(2026, 9, 3, 20, 57, 30, 0, time.FixedZone("local", 4*60*60)))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 3, 16, 58, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("Next() = %s, want %s UTC", got, want)
	}
}

func TestParseRejectsExtendedCronSyntax(t *testing.T) {
	for _, expression := range []string{"@hourly", "CRON_TZ=Europe/Berlin 0 * * * *", "TZ=UTC 0 * * * *", "0 0 0 * * *", " 0 * * * *"} {
		t.Run(expression, func(t *testing.T) {
			if _, err := Parse(expression); err == nil {
				t.Fatalf("Parse(%q) error = nil", expression)
			}
		})
	}
}
