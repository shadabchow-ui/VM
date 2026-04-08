package db

// event_repo.go — Instance event log persistence.
//
// Source: EVENTS_SCHEMA_V1 §2 (struct), §4 (17 event type constants),
//         IMPLEMENTATION_PLAN_V1 §R-17 (usage events written in same DB transaction as state changes).
//
// Retention: last 100 events per instance. Enforced by a background purge job (M8).

import (
	"context"
	"fmt"
	"time"
)

// EventRow is the DB representation of an instance_events row.
type EventRow struct {
	ID         string
	InstanceID string
	EventType  string
	Message    string
	Actor      string
	Details    []byte // JSONB, nil if no structured details
	CreatedAt  time.Time
}

// Event type constants. Source: EVENTS_SCHEMA_V1 §4.
const (
	EventInstanceCreate            = "instance.create"
	EventInstanceProvisioningStart = "instance.provisioning.start"
	EventInstanceProvisioningDone  = "instance.provisioning.done"
	EventInstanceStart             = "instance.start"
	EventInstanceStartInitiate     = "instance.start.initiate"
	EventInstanceStop              = "instance.stop"
	EventInstanceStopInitiate      = "instance.stop.initiate"
	EventInstanceReboot            = "instance.reboot"
	EventInstanceRebootInitiate    = "instance.reboot.initiate"
	EventInstanceDeleteInitiate    = "instance.delete.initiate"
	EventInstanceDelete            = "instance.delete"
	EventInstanceFailure           = "instance.failure"
	EventUsageStart                = "usage.start"
	EventUsageEnd                  = "usage.end"
	EventSSHKeyAttached            = "instance.ssh_key.attached"
	EventIPAllocated               = "instance.ip.allocated"
	EventIPReleased                = "instance.ip.released"
	// EventIPUniquenessViolation is written by the IP uniqueness reconciler
	// sub-scan when it detects that two instances share the same IP (invariant I-2
	// violated). Written once per affected instance per scan cycle.
	// Source: IMPLEMENTATION_PLAN_V1 §M6 gate, IP_ALLOCATION_CONTRACT_V1.
	EventIPUniquenessViolation = "instance.ip.uniqueness_violation"
)

// InsertEvent appends an event to the instance event log.
// Should be called within the same transaction as the state change it records.
// Source: IMPLEMENTATION_PLAN_V1 §R-17.
func (r *Repo) InsertEvent(ctx context.Context, row *EventRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO instance_events (id, instance_id, event_type, message, actor, details, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`,
		row.ID, row.InstanceID, row.EventType,
		row.Message, row.Actor, row.Details,
	)
	if err != nil {
		return fmt.Errorf("InsertEvent: %w", err)
	}
	return nil
}

// ListEvents returns the most recent events for an instance, newest first.
// Limit is capped at 100. Source: EVENTS_SCHEMA_V1 (retention: last 100 events).
func (r *Repo) ListEvents(ctx context.Context, instanceID string, limit int) ([]*EventRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, instance_id, event_type, message, actor, details, created_at
		FROM instance_events
		WHERE instance_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, instanceID, limit)
	if err != nil {
		return nil, fmt.Errorf("ListEvents: %w", err)
	}
	defer rows.Close()

	var out []*EventRow
	for rows.Next() {
		row := &EventRow{}
		if err := rows.Scan(
			&row.ID, &row.InstanceID, &row.EventType,
			&row.Message, &row.Actor, &row.Details, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListEvents scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
