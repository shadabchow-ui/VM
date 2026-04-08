package db

// instance_repo.go — Instance table persistence methods.
//
// Source: INSTANCE_MODEL_V1 §2 (API fields), §4 (DB columns),
//         LIFECYCLE_STATE_MACHINE_V1 (state transitions),
//         core-architecture-blueprint §optimistic locking (version column).
//
// State mutation rule: all UPDATE calls that change vm_state MUST use
// WHERE version = $n AND vm_state = $expected to enforce optimistic locking.
// A 0-row result means concurrent modification — caller retries or fails the job.

import (
	"context"
	"fmt"
	"time"
)

// InstanceRow is the DB representation of an instance record.
type InstanceRow struct {
	ID               string
	Name             string
	OwnerPrincipalID string
	VMState          string
	InstanceTypeID   string
	ImageID          string
	HostID           *string
	AvailabilityZone string
	Version          int
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// InsertInstance writes a new instance record in the 'requested' state.
// Source: INSTANCE_MODEL_V1 §4, 04-01-create-instance-flow.md.
func (r *Repo) InsertInstance(ctx context.Context, row *InstanceRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO instances (
			id, name, owner_principal_id, vm_state,
			instance_type_id, image_id, availability_zone,
			version, created_at, updated_at
		) VALUES ($1,$2,$3,'requested',$4,$5,$6,0,NOW(),NOW())
	`,
		row.ID, row.Name, row.OwnerPrincipalID,
		row.InstanceTypeID, row.ImageID, row.AvailabilityZone,
	)
	if err != nil {
		return fmt.Errorf("InsertInstance: %w", err)
	}
	return nil
}

// GetInstanceByID fetches a single instance. Returns ErrHostNotFound pattern
// (caller checks for pgx.ErrNoRows equivalent).
func (r *Repo) GetInstanceByID(ctx context.Context, id string) (*InstanceRow, error) {
	row := &InstanceRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, owner_principal_id, vm_state,
		       instance_type_id, image_id, host_id, availability_zone,
		       version, created_at, updated_at, deleted_at
		FROM instances
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id).Scan(
		&row.ID, &row.Name, &row.OwnerPrincipalID, &row.VMState,
		&row.InstanceTypeID, &row.ImageID, &row.HostID, &row.AvailabilityZone,
		&row.Version, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("GetInstanceByID %s: %w", id, err)
	}
	return row, nil
}

// UpdateInstanceState transitions an instance to a new state using optimistic locking.
// The UPDATE only applies if the current version and state match expectations.
// Returns an error if 0 rows were updated (concurrent modification detected).
// Source: core-architecture-blueprint §optimistic locking, LIFECYCLE_STATE_MACHINE_V1.
func (r *Repo) UpdateInstanceState(ctx context.Context, id, expectedState, newState string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE instances
		SET vm_state   = $3,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id        = $1
		  AND vm_state  = $2
		  AND version   = $4
		  AND deleted_at IS NULL
	`, id, expectedState, newState, version)
	if err != nil {
		return fmt.Errorf("UpdateInstanceState: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateInstanceState: concurrent modification or state mismatch for instance %s", id)
	}
	return nil
}

// AssignHost sets the host_id on an instance during provisioning.
// Called by the INSTANCE_CREATE worker after the scheduler selects a host.
func (r *Repo) AssignHost(ctx context.Context, instanceID, hostID string, version int) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE instances
		SET host_id    = $2,
		    version    = version + 1,
		    updated_at = NOW()
		WHERE id      = $1
		  AND version = $3
		  AND deleted_at IS NULL
	`, instanceID, hostID, version)
	if err != nil {
		return fmt.Errorf("AssignHost: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("AssignHost: concurrent modification for instance %s", instanceID)
	}
	return nil
}

// ListInstancesByOwner returns all non-deleted instances for a principal, newest first.
// Source: AUTH_OWNERSHIP_MODEL_V1 §4 (ownership check), 08-01 §ListInstances.
func (r *Repo) ListInstancesByOwner(ctx context.Context, ownerPrincipalID string) ([]*InstanceRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, owner_principal_id, vm_state,
		       instance_type_id, image_id, host_id, availability_zone,
		       version, created_at, updated_at, deleted_at
		FROM instances
		WHERE owner_principal_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, ownerPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("ListInstancesByOwner: %w", err)
	}
	defer rows.Close()

	var out []*InstanceRow
	for rows.Next() {
		row := &InstanceRow{}
		if err := rows.Scan(
			&row.ID, &row.Name, &row.OwnerPrincipalID, &row.VMState,
			&row.InstanceTypeID, &row.ImageID, &row.HostID, &row.AvailabilityZone,
			&row.Version, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListInstancesByOwner scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SoftDeleteInstance marks an instance as deleted (sets deleted_at).
// Called after the INSTANCE_DELETE job completes successfully.
// Source: INSTANCE_MODEL_V1 §4 (deleted_at soft-delete pattern).
func (r *Repo) SoftDeleteInstance(ctx context.Context, id string, version int) error {
	now := time.Now().UTC()
	tag, err := r.pool.Exec(ctx, `
		UPDATE instances
		SET vm_state   = 'deleted',
		    deleted_at = $2,
		    version    = version + 1,
		    updated_at = $2
		WHERE id      = $1
		  AND version = $3
		  AND deleted_at IS NULL
	`, id, now, version)
	if err != nil {
		return fmt.Errorf("SoftDeleteInstance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteInstance: concurrent modification for instance %s", id)
	}
	return nil
}
