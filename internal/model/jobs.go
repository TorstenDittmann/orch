package model

// Job retains an immutable one-off template and controller-owned run identity.
// Template contains secret references only and is never returned by read APIs.
type Job struct {
	ID              string
	NodeID          string
	Service         string
	Kind            string
	RetryOf         string
	Template        CronJobDesired
	Status          JobRunStatus
	CancelRequested bool
}
