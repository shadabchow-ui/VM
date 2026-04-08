package domainmodel

import "time"

// JobType enumerates all valid job types. Source: JOB_MODEL_V1 §3.
type JobType string

const (
	JobTypeInstanceCreate  JobType = "INSTANCE_CREATE"
	JobTypeInstanceDelete  JobType = "INSTANCE_DELETE"
	JobTypeInstanceStart   JobType = "INSTANCE_START"
	JobTypeInstanceStop    JobType = "INSTANCE_STOP"
	JobTypeInstanceReboot  JobType = "INSTANCE_REBOOT"
)

// JobStatus enumerates job lifecycle states. Source: JOB_MODEL_V1 §2.
type JobStatus string

const (
	JobStatusPending    JobStatus = "pending"
	JobStatusInProgress JobStatus = "in_progress"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
	JobStatusDead       JobStatus = "dead"
)

// Job is the canonical async job object. Source: JOB_MODEL_V1 §1.
type Job struct {
	ID               string     `db:"id"`
	InstanceID       string     `db:"instance_id"`
	Type             JobType    `db:"job_type"`
	Status           JobStatus  `db:"status"`
	IdempotencyKey   string     `db:"idempotency_key"`
	AttemptCount     int        `db:"attempt_count"`
	MaxAttempts      int        `db:"max_attempts"`
	ErrorMessage     *string    `db:"error_message"`
	CreatedAt        time.Time  `db:"created_at"`
	UpdatedAt        time.Time  `db:"updated_at"`
	ClaimedAt        *time.Time `db:"claimed_at"`
	CompletedAt      *time.Time `db:"completed_at"`
}
