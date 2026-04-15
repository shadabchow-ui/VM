package db

// project_membership_repo.go — project_members persistence and project-aware
// instance visibility queries for VM-P2D Slice 2 (RBAC enforcement foundation).
//
// Source: P2_PROJECT_RBAC_MODEL.md §3 (membership model),
//         §4.2 (permission matrix — all roles permit read),
//         §4.3 (permission evaluation order),
//         AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account),
//         AUTH_OWNERSHIP_MODEL_V1 §4 (authorization check order).
//
// Design constraints:
//   - Phase 1 ListInstancesByOwner is unchanged — callers that only need
//     direct-ownership listing continue to work without modification.
//   - ListInstancesVisible is additive: it is a strict superset of
//     ListInstancesByOwner for accounts with no project memberships.
//   - No quota, capacity admission, scheduler, or network-controller changes.
//   - 404-for-cross-account is preserved by callers: these methods return
//     false/empty for non-visible resources, never exposing existence.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MembershipRole is one of the three valid project role values.
// Source: P2_PROJECT_RBAC_MODEL.md §4.1.
type MembershipRole string

const (
	RoleOwner  MembershipRole = "OWNER"
	RoleEditor MembershipRole = "EDITOR"
	RoleViewer MembershipRole = "VIEWER"
)

// MembershipRow is the DB projection of a project_members row.
// Column order matches the SELECT in GetProjectMember.
type MembershipRow struct {
	ID                 string
	ProjectID          string
	AccountPrincipalID string
	Role               MembershipRole
	InvitedBy          *string
	JoinedAt           time.Time
	RemovedAt          *time.Time
}

// GetProjectMember returns the active membership for (projectID, accountPrincipalID).
// Returns an error wrapping sql.ErrNoRows when no active membership exists.
//
// Used by the ownership resolution path to confirm a project member's role.
// Source: P2_PROJECT_RBAC_MODEL.md §4.3 step 4.
//
// SQL pattern (memPool dispatch key):
//
//	"FROM project_members" AND "project_id" AND "account_principal_id"
func (r *Repo) GetProjectMember(ctx context.Context, projectID, accountPrincipalID string) (*MembershipRow, error) {
	const q = `
SELECT id, project_id, account_principal_id, role, invited_by, joined_at, removed_at
FROM project_members
WHERE project_id           = $1
  AND account_principal_id = $2
  AND removed_at IS NULL`
	m := &MembershipRow{}
	if err := r.pool.QueryRow(ctx, q, projectID, accountPrincipalID).Scan(
		&m.ID, &m.ProjectID, &m.AccountPrincipalID, &m.Role,
		&m.InvitedBy, &m.JoinedAt, &m.RemovedAt,
	); err != nil {
		return nil, fmt.Errorf("GetProjectMember: %w", err)
	}
	return m, nil
}

// AddProjectMember inserts a new active membership row.
// Duplicate active memberships are rejected by idx_project_members_unique;
// callers should treat that error as 409 Conflict.
//
// SQL pattern (memPool dispatch key):
//
//	"INSERT INTO project_members"
func (r *Repo) AddProjectMember(
	ctx context.Context,
	memberID, projectID, accountPrincipalID string,
	role MembershipRole,
	invitedBy *string,
) error {
	const q = `
INSERT INTO project_members (id, project_id, account_principal_id, role, invited_by)
VALUES ($1, $2, $3, $4, $5)`
	if _, err := r.pool.Exec(ctx, q, memberID, projectID, accountPrincipalID, string(role), invitedBy); err != nil {
		return fmt.Errorf("AddProjectMember: %w", err)
	}
	return nil
}

// ListInstancesVisible returns all non-deleted instances that accountPrincipalID
// is authorized to see, newest first:
//
//  1. Instances directly owned by accountPrincipalID (Phase 1 path, unchanged).
//  2. Instances owned by a PROJECT principal where accountPrincipalID has an
//     active membership (all roles: OWNER, EDITOR, VIEWER permit read).
//
// This is the replacement for ListInstancesByOwner in list endpoints after
// VM-P2D Slice 2. Callers must not expose the returned list as proof of
// existence for instances outside this set.
//
// Source: P2_PROJECT_RBAC_MODEL.md §4.2 (Read any project resource: all roles),
//         §4.3 (permission evaluation order),
//         AUTH_OWNERSHIP_MODEL_V1 §3 (ownership hiding).
//
// SQL patterns (memPool dispatch keys):
//
//	"project_members" AND "principal_id"  — first query (project principal_ids)
//	"FROM instances" AND "ANY("           — second query (instance rows)
func (r *Repo) ListInstancesVisible(ctx context.Context, accountPrincipalID string) ([]*InstanceRow, error) {
	// Step 1: collect project principal_ids for projects the account is a member of.
	// These are the owner_principal_id values stored on project-assigned instances.
	const qProjects = `
SELECT p.principal_id
FROM project_members pm
JOIN projects p ON p.id = pm.project_id
WHERE pm.account_principal_id = $1
  AND pm.removed_at IS NULL
  AND p.deleted_at IS NULL`
	pRows, err := r.pool.Query(ctx, qProjects, accountPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("ListInstancesVisible projects: %w", err)
	}
	defer pRows.Close()

	var projectPrincipalIDs []string
	for pRows.Next() {
		var pid string
		if err := pRows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("ListInstancesVisible projects scan: %w", err)
		}
		projectPrincipalIDs = append(projectPrincipalIDs, pid)
	}
	if err := pRows.Err(); err != nil {
		return nil, fmt.Errorf("ListInstancesVisible projects rows: %w", err)
	}

	// Step 2: build the full owner set and fetch instances.
	ownerSet := make([]string, 0, 1+len(projectPrincipalIDs))
	ownerSet = append(ownerSet, accountPrincipalID)
	ownerSet = append(ownerSet, projectPrincipalIDs...)

	const qInstances = `
SELECT id, name, owner_principal_id, vm_state,
       instance_type_id, image_id, host_id, availability_zone,
       version, created_at, updated_at, deleted_at
FROM instances
WHERE owner_principal_id = ANY($1::text[])
  AND deleted_at IS NULL
ORDER BY created_at DESC`
	iRows, err := r.pool.Query(ctx, qInstances, ownerSet)
	if err != nil {
		return nil, fmt.Errorf("ListInstancesVisible: %w", err)
	}
	defer iRows.Close()

	var out []*InstanceRow
	for iRows.Next() {
		row := &InstanceRow{}
		if err := iRows.Scan(
			&row.ID, &row.Name, &row.OwnerPrincipalID, &row.VMState,
			&row.InstanceTypeID, &row.ImageID, &row.HostID, &row.AvailabilityZone,
			&row.Version, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListInstancesVisible scan: %w", err)
		}
		out = append(out, row)
	}
	return out, iRows.Err()
}

// IsInstanceVisibleTo reports whether instanceID is readable by accountPrincipalID.
// Returns false (not error) when the instance does not exist, is soft-deleted,
// or the caller has no access. Callers must return 404 in all false cases.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §4 (authorization check),
//         P2_PROJECT_RBAC_MODEL.md §4.3 steps 3–5.
//
// SQL patterns (memPool dispatch keys):
//
//	"SELECT EXISTS" AND "project_members" AND "principal_id" (without "IN ('OWNER','EDITOR')")
func (r *Repo) IsInstanceVisibleTo(ctx context.Context, instanceID, accountPrincipalID string) (bool, error) {
	inst, err := r.GetInstanceByID(ctx, instanceID)
	if err != nil {
		if isNoRowsErr(err) {
			return false, nil
		}
		return false, err
	}

	// Step 3: direct account ownership (Phase 1 model).
	if inst.OwnerPrincipalID == accountPrincipalID {
		return true, nil
	}

	// Step 4: project membership — any role permits read.
	const q = `
SELECT EXISTS(
    SELECT 1
    FROM projects p
    JOIN project_members pm ON pm.project_id = p.id
    WHERE p.principal_id           = $1
      AND pm.account_principal_id  = $2
      AND pm.removed_at IS NULL
      AND p.deleted_at  IS NULL
)`
	var allowed bool
	if err := r.pool.QueryRow(ctx, q, inst.OwnerPrincipalID, accountPrincipalID).Scan(&allowed); err != nil {
		return false, fmt.Errorf("IsInstanceVisibleTo: %w", err)
	}
	return allowed, nil
}

// IsInstanceWritableBy reports whether accountPrincipalID has write permission
// on instanceID: direct account ownership, or OWNER/EDITOR project membership.
//
// VIEWER members can read but not write. This method returns false for VIEWER.
// Callers distinguish VIEWER (visible + not writable) from non-member (not visible)
// by calling IsInstanceVisibleTo when this method returns false.
//
// Source: P2_PROJECT_RBAC_MODEL.md §4.2 (permission matrix).
//
// SQL patterns (memPool dispatch keys):
//
//	"SELECT EXISTS" AND "project_members" AND "principal_id" AND "IN ('OWNER','EDITOR')"
func (r *Repo) IsInstanceWritableBy(ctx context.Context, instanceID, accountPrincipalID string) (bool, error) {
	inst, err := r.GetInstanceByID(ctx, instanceID)
	if err != nil {
		if isNoRowsErr(err) {
			return false, nil
		}
		return false, err
	}

	// Direct account ownership → full write access (Phase 1 model).
	if inst.OwnerPrincipalID == accountPrincipalID {
		return true, nil
	}

	// Project membership with OWNER or EDITOR role.
	const q = `
SELECT EXISTS(
    SELECT 1
    FROM projects p
    JOIN project_members pm ON pm.project_id = p.id
    WHERE p.principal_id           = $1
      AND pm.account_principal_id  = $2
      AND pm.role                  IN ('OWNER','EDITOR')
      AND pm.removed_at IS NULL
      AND p.deleted_at  IS NULL
)`
	var allowed bool
	if err := r.pool.QueryRow(ctx, q, inst.OwnerPrincipalID, accountPrincipalID).Scan(&allowed); err != nil {
		return false, fmt.Errorf("IsInstanceWritableBy: %w", err)
	}
	return allowed, nil
}

// isNoRowsErr matches the "no rows" sentinel from both pgx and the memPool test fake.
// Kept package-private to this file; callers in other files use isNoRows (handlers pkg).
func isNoRowsErr(err error) bool {
	if err == nil {
		return false
	}
	if err == sql.ErrNoRows {
		return true
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
