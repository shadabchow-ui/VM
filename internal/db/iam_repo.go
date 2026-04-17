package db

// iam_repo.go — Phase 16B: Service Account and Role Binding persistence.
//
// Implements the exact API required by internal/db/iam_repo_test.go:
//
//   Service accounts:
//     CreateServiceAccount(ctx, id, projectID, name, displayName, createdBy, description) → (*ServiceAccountRow, error)
//     GetServiceAccountByID(ctx, id, projectID) → (*ServiceAccountRow, error)
//     ListServiceAccountsByProject(ctx, projectID) → ([]*ServiceAccountRow, error)
//     SetServiceAccountStatus(ctx, id, projectID, status) → (*ServiceAccountRow, error)
//     SoftDeleteServiceAccount(ctx, id, projectID) → error
//
//   Role bindings:
//     CreateRoleBinding(ctx, id, projectID, principalID, role, resourceType, resourceID, grantedBy) → (*RoleBindingRow, error)
//     GetRoleBindingByID(ctx, id, projectID) → (*RoleBindingRow, error)
//     ListRoleBindings(ctx, projectID, principalID) → ([]*RoleBindingRow, error)
//     DeleteRoleBinding(ctx, id, projectID) → error
//     CheckPrincipalHasRole(ctx, projectID, principalID, role, resourceType, resourceID) → (bool, error)
//
// Domain errors:
//   ErrServiceAccountNotFound      — SA not found or cross-project access
//   ErrServiceAccountNameConflict  — duplicate (project_id, name)
//   ErrRoleBindingNotFound         — binding not found or cross-project access
//
// IAM constants:
//   IAMRoleOwner, IAMRoleComputeViewer
//   IAMResourceTypeProject, IAMResourceTypeServiceAccount
//
// Ownership contract: all mutations and reads are project-scoped.
// A lookup with the wrong project_id returns ErrServiceAccountNotFound /
// ErrRoleBindingNotFound — never 403, per AUTH_OWNERSHIP_MODEL_V1 §3.
//
// Source: vm-16-01__blueprint__ §mvp, §core_contracts,
//         AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account),
//         API_ERROR_CONTRACT_V1 §4 (error codes).

import (
	"strings"
	"context"
	"errors"
	"fmt"
	"time"
)

// ── Domain errors ─────────────────────────────────────────────────────────────

// ErrServiceAccountNotFound is returned when a service account lookup fails
// because the SA does not exist, is soft-deleted, or belongs to a different
// project (cross-project access is hidden as 404, not 403).
// Source: AUTH_OWNERSHIP_MODEL_V1 §3, vm-16-01__blueprint__ §core_contracts.
var ErrServiceAccountNotFound = errors.New("service account not found")

// ErrServiceAccountNameConflict is returned when an INSERT fails due to a
// duplicate (project_id, name) unique constraint violation.
// Source: vm-16-01__blueprint__ §core_contracts "Resources have a Single, Immutable Parent".
var ErrServiceAccountNameConflict = errors.New("service account name already exists in this project")

// ErrRoleBindingNotFound is returned when a role binding lookup fails because
// the binding does not exist or belongs to a different project.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
var ErrRoleBindingNotFound = errors.New("role binding not found")

// ── IAM role and resource type constants ──────────────────────────────────────

// Source: vm-16-01__blueprint__ §core_contracts "Authorization is Hierarchical and Additive".
const (
	// IAMRoleOwner grants full create/delete/manage permissions on a project.
	IAMRoleOwner = "roles/owner"

	// IAMRoleComputeViewer grants list and get permissions on compute resources.
	IAMRoleComputeViewer = "roles/compute.viewer"

	// IAMResourceTypeProject is the resource_type value for project-level bindings.
	IAMResourceTypeProject = "project"

	// IAMResourceTypeServiceAccount is the resource_type for SA-level bindings.
	IAMResourceTypeServiceAccount = "service_account"
)

// ── ServiceAccountRow ─────────────────────────────────────────────────────────

// ServiceAccountRow is the DB projection of a service_accounts row.
//
// Column order is fixed and matches all SELECT queries in this file and the
// saRow helper in iam_repo_test.go:
//
//	0: id           string
//	1: project_id   string
//	2: name         string
//	3: display_name string
//	4: description  *string
//	5: status       string
//	6: created_by   string
//	7: created_at   time.Time
//	8: updated_at   time.Time
//	9: deleted_at   *time.Time
type ServiceAccountRow struct {
	ID          string
	ProjectID   string
	Name        string
	DisplayName string
	Description *string
	Status      string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// ── Service account repo methods ──────────────────────────────────────────────

// CreateServiceAccount inserts a new service account row and returns the freshly
// fetched record. Returns ErrServiceAccountNameConflict on a unique constraint
// violation (duplicate name within the project).
//
// Source: vm-16-01__blueprint__ §mvp "static keys only".
func (r *Repo) CreateServiceAccount(
	ctx context.Context,
	id, projectID, name, displayName, createdBy string,
	description *string,
) (*ServiceAccountRow, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO service_accounts (
			id, project_id, name, display_name, description,
			status, created_by, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, 'active', $6, NOW(), NOW())
	`, id, projectID, name, displayName, description, createdBy)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("CreateServiceAccount: %w", ErrServiceAccountNameConflict)
		}
		return nil, fmt.Errorf("CreateServiceAccount: %w", err)
	}
	return r.GetServiceAccountByID(ctx, id, projectID)
}

// GetServiceAccountByID fetches a non-deleted service account by (id, project_id).
// Returns ErrServiceAccountNotFound when absent, deleted, or cross-project.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (ownership hiding).
func (r *Repo) GetServiceAccountByID(ctx context.Context, id, projectID string) (*ServiceAccountRow, error) {
	sa := &ServiceAccountRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, project_id, name, display_name, description,
		       status, created_by, created_at, updated_at, deleted_at
		FROM service_accounts
		WHERE id         = $1
		  AND project_id = $2
		  AND deleted_at IS NULL
	`, id, projectID).Scan(
		&sa.ID, &sa.ProjectID, &sa.Name, &sa.DisplayName, &sa.Description,
		&sa.Status, &sa.CreatedBy, &sa.CreatedAt, &sa.UpdatedAt, &sa.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, fmt.Errorf("GetServiceAccountByID %s: %w", id, ErrServiceAccountNotFound)
		}
		return nil, fmt.Errorf("GetServiceAccountByID: %w", err)
	}
	return sa, nil
}

// ListServiceAccountsByProject returns all non-deleted service accounts for a project.
func (r *Repo) ListServiceAccountsByProject(ctx context.Context, projectID string) ([]*ServiceAccountRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, project_id, name, display_name, description,
		       status, created_by, created_at, updated_at, deleted_at
		FROM service_accounts
		WHERE project_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("ListServiceAccountsByProject: %w", err)
	}
	defer rows.Close()

	var out []*ServiceAccountRow
	for rows.Next() {
		sa := &ServiceAccountRow{}
		if err := rows.Scan(
			&sa.ID, &sa.ProjectID, &sa.Name, &sa.DisplayName, &sa.Description,
			&sa.Status, &sa.CreatedBy, &sa.CreatedAt, &sa.UpdatedAt, &sa.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListServiceAccountsByProject scan: %w", err)
		}
		out = append(out, sa)
	}
	if out == nil {
		out = []*ServiceAccountRow{}
	}
	return out, rows.Err()
}

// SetServiceAccountStatus updates the status of a service account and returns the
// updated record. Returns ErrServiceAccountNotFound when no row is updated (wrong
// project_id or SA not found).
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (project-scoped mutation).
func (r *Repo) SetServiceAccountStatus(ctx context.Context, id, projectID, status string) (*ServiceAccountRow, error) {
	res, err := r.pool.Exec(ctx, `
		UPDATE service_accounts
		SET status     = $3,
		    updated_at = NOW()
		WHERE id         = $1
		  AND project_id = $2
		  AND deleted_at IS NULL
	`, id, projectID, status)
	if err != nil {
		return nil, fmt.Errorf("SetServiceAccountStatus: %w", err)
	}
	if res.RowsAffected() == 0 {
		return nil, fmt.Errorf("SetServiceAccountStatus %s: %w", id, ErrServiceAccountNotFound)
	}
	return r.GetServiceAccountByID(ctx, id, projectID)
}

// SoftDeleteServiceAccount sets deleted_at on a service account.
// Returns ErrServiceAccountNotFound when the SA does not exist, is already
// deleted, or belongs to a different project.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func (r *Repo) SoftDeleteServiceAccount(ctx context.Context, id, projectID string) error {
	res, err := r.pool.Exec(ctx, `
		UPDATE service_accounts
		SET deleted_at = NOW(),
		    updated_at = NOW()
		WHERE id         = $1
		  AND project_id = $2
		  AND deleted_at IS NULL
	`, id, projectID)
	if err != nil {
		return fmt.Errorf("SoftDeleteServiceAccount: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteServiceAccount %s: %w", id, ErrServiceAccountNotFound)
	}
	return nil
}

// ── RoleBindingRow ─────────────────────────────────────────────────────────────

// RoleBindingRow is the DB projection of a role_bindings row.
//
// Column order is fixed and matches all SELECT queries in this file and the
// role binding assertions in iam_repo_test.go:
//
//	0: id              string
//	1: project_id      string
//	2: principal_id    string
//	3: role            string
//	4: resource_type   string
//	5: resource_id     string
//	6: granted_by      string
//	7: created_at      time.Time
//	8: updated_at      time.Time
type RoleBindingRow struct {
	ID           string
	ProjectID    string
	PrincipalID  string
	Role         string
	ResourceType string
	ResourceID   string
	GrantedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ── Role binding repo methods ─────────────────────────────────────────────────

// CreateRoleBinding inserts a new role binding with ON CONFLICT DO NOTHING.
// When the binding already exists (conflict), RowsAffected == 0 and the method
// calls GetRoleBindingByID to return the existing row. If GetRoleBindingByID
// returns not-found after a conflict, the caller receives nil, ErrRoleBindingNotFound.
//
// Source: vm-16-01__blueprint__ §core_contracts "Authorization is Hierarchical and Additive".
func (r *Repo) CreateRoleBinding(
	ctx context.Context,
	id, projectID, principalID, role, resourceType, resourceID, grantedBy string,
) (*RoleBindingRow, error) {
	res, err := r.pool.Exec(ctx, `
		INSERT INTO role_bindings (
			id, project_id, principal_id, role,
			resource_type, resource_id, granted_by,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		ON CONFLICT (project_id, principal_id, role, resource_type, resource_id) DO NOTHING
	`, id, projectID, principalID, role, resourceType, resourceID, grantedBy)
	if err != nil {
		return nil, fmt.Errorf("CreateRoleBinding: %w", err)
	}
	if res.RowsAffected() == 0 {
		// Binding already exists — return the existing row.
		return r.GetRoleBindingByID(ctx, id, projectID)
	}
	return r.GetRoleBindingByID(ctx, id, projectID)
}

// GetRoleBindingByID fetches a role binding by (id, project_id).
// Returns ErrRoleBindingNotFound when absent or cross-project.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func (r *Repo) GetRoleBindingByID(ctx context.Context, id, projectID string) (*RoleBindingRow, error) {
	rb := &RoleBindingRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, project_id, principal_id, role,
		       resource_type, resource_id, granted_by,
		       created_at, updated_at
		FROM role_bindings
		WHERE id         = $1
		  AND project_id = $2
	`, id, projectID).Scan(
		&rb.ID, &rb.ProjectID, &rb.PrincipalID, &rb.Role,
		&rb.ResourceType, &rb.ResourceID, &rb.GrantedBy,
		&rb.CreatedAt, &rb.UpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, fmt.Errorf("GetRoleBindingByID %s: %w", id, ErrRoleBindingNotFound)
		}
		return nil, fmt.Errorf("GetRoleBindingByID: %w", err)
	}
	return rb, nil
}

// ListRoleBindings returns role bindings for a project, optionally filtered by
// principalID. Pass empty string for principalID to return all bindings.
func (r *Repo) ListRoleBindings(ctx context.Context, projectID, principalID string) ([]*RoleBindingRow, error) {
	var (
		rows Rows
		err  error
	)
	if principalID == "" {
		rows, err = r.pool.Query(ctx, `
			SELECT id, project_id, principal_id, role,
			       resource_type, resource_id, granted_by,
			       created_at, updated_at
			FROM role_bindings
			WHERE project_id = $1
			ORDER BY created_at ASC
		`, projectID)
	} else {
		rows, err = r.pool.Query(ctx, `
			SELECT id, project_id, principal_id, role,
			       resource_type, resource_id, granted_by,
			       created_at, updated_at
			FROM role_bindings
			WHERE project_id   = $1
			  AND principal_id = $2
			ORDER BY created_at ASC
		`, projectID, principalID)
	}
	if err != nil {
		return nil, fmt.Errorf("ListRoleBindings: %w", err)
	}
	defer rows.Close()

	var out []*RoleBindingRow
	for rows.Next() {
		rb := &RoleBindingRow{}
		if err := rows.Scan(
			&rb.ID, &rb.ProjectID, &rb.PrincipalID, &rb.Role,
			&rb.ResourceType, &rb.ResourceID, &rb.GrantedBy,
			&rb.CreatedAt, &rb.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListRoleBindings scan: %w", err)
		}
		out = append(out, rb)
	}
	return out, rows.Err()
}

// DeleteRoleBinding removes a binding by (id, project_id).
// Returns ErrRoleBindingNotFound when no row is deleted (wrong project or absent).
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func (r *Repo) DeleteRoleBinding(ctx context.Context, id, projectID string) error {
	res, err := r.pool.Exec(ctx, `
		DELETE FROM role_bindings
		WHERE id         = $1
		  AND project_id = $2
	`, id, projectID)
	if err != nil {
		return fmt.Errorf("DeleteRoleBinding: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("DeleteRoleBinding %s: %w", id, ErrRoleBindingNotFound)
	}
	return nil
}

// CheckPrincipalHasRole returns true when a role binding exists for the given
// (projectID, principalID, role, resourceType, resourceID) tuple.
//
// This is the central IAM check used by handlers before admitting a request.
// Source: vm-16-01__blueprint__ §core_contracts "Authorization is Hierarchical and Additive".
func (r *Repo) CheckPrincipalHasRole(
	ctx context.Context,
	projectID, principalID, role, resourceType, resourceID string,
) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM role_bindings
			WHERE project_id   = $1
			  AND principal_id = $2
			  AND role         = $3
			  AND resource_type = $4
			  AND resource_id   = $5
		)
	`, projectID, principalID, role, resourceType, resourceID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("CheckPrincipalHasRole: %w", err)
	}
	return exists, nil
}

// isNoRowsErr and isDuplicateKeyErr are defined in internal/db/helpers.go
// (or equivalent) in the real repo. They are used here but NOT redeclared.
// If the real repo does not have a helpers.go that defines these, apply
// internal/db/db_helpers.go from this bundle.
