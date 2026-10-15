package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const MaxExpressionBytes = 256

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Parse accepts exactly the standard five-field cron grammar. Schedules are
// evaluated in UTC by callers; embedded timezone directives and descriptors
// are deliberately outside the application contract.
func Parse(expression string) (cron.Schedule, error) {
	if expression == "" || strings.TrimSpace(expression) != expression || len(expression) > MaxExpressionBytes {
		return nil, fmt.Errorf("cron schedule must be nonempty, canonical, and at most %d bytes", MaxExpressionBytes)
	}
	if strings.HasPrefix(expression, "CRON_TZ=") || strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "@") {
		return nil, fmt.Errorf("cron schedule must use five UTC fields without timezone directives or descriptors")
	}
	if len(strings.Fields(expression)) != 5 {
		return nil, fmt.Errorf("cron schedule must contain exactly five fields")
	}
	schedule, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("parse five-field cron schedule: %w", err)
	}
	return schedule, nil
}

func Next(expression string, after time.Time) (time.Time, error) {
	schedule, err := Parse(expression)
	if err != nil {
		return time.Time{}, err
	}
	// robfig/cron reports a schedule without any matching calendar date (for
	// example "0 0 30 2 *") as the zero time instead of an error.
	next := schedule.Next(after.UTC())
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("cron schedule never fires: no calendar date matches it")
	}
	return next.UTC(), nil
}
