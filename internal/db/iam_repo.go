package db

// iam_repo.go — Service Account and IAM Role Binding persistence.
//
// Phase 16A: Smallest correct repo-native seams for the tenancy / authorization
// model described in vm-16-01__blueprint__.
//
// What this file adds:
//   - ServiceAccountRow  — DB projection of service_accounts
//   - IAMRoleBindingRow  — DB projection of iam_role_bindings
//   - Repo methods for CRUD on both tables
//   - CheckPrincipalHasRole — Phase 16A authorization check seam (flat check;
//     Phase 16B extends to hierarchical materialized-path traversal)
//
// What is intentionally deferred to Phase 16B / future phases:
//   - IAM Policy Service (check(principal, permission, resource_name) endpoint)
//   - Materialized path hierarchy traversal (Org → Folder → Project)
//   - IAM Credentials Service (token vending / impersonation)
//   - Folder support (flat org→project is sufficient for Phase 16A)
//   - Organization Policy Service (deny overrides, org-wide guardrails)
//
// Ownership semantics preserved:
//   - Cross-account access always surfaces as not-found (404, not 403).
//   - Service accounts are scoped to a project (project_id FK).
//   - Role bindings are scoped to (project_id, principal_id, role, resource_type, resource_id).
//
// Schema requirements (migration 017_phase16a_iam.up.sql):
//
//	CREATE TABLE service_accounts (
//	  id           TEXT PRIMARY KEY,
//	  project_id   TEXT NOT NULL REFERENCES projects(id),
//	  name         TEXT NOT NULL,
//	  display_name TEXT NOT NULL,
//	  description  TEXT,
//	  status       TEXT NOT NULL DEFAULT 'active',
//	  created_by   TEXT NOT NULL,
//	  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	  deleted_at   TIMESTAMPTZ,
//	  UNIQUE (project_id, name)
//	);
//
//	CREATE TABLE iam_role_bindings (
//	  id            TEXT PRIMARY KEY,
//	  project_id    TEXT NOT NULL REFERENCES projects(id),
//	  principal_id  TEXT NOT NULL,
//	  role          TEXT NOT NULL,
//	  resource_type TEXT NOT NULL,
//	  resource_id   TEXT NOT NULL,
//	  granted_by    TEXT NOT NULL,
//	  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	  UNIQUE (project_id, principal_id, role, resource_type, resource_id)
//	);
//
// Source: vm-16-01__blueprint__ §components, §core_contracts,
//
//	vm-16-01__research__ §"Identity Principals", §"Authorization Model",
//	AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ── ServiceAccountRow ─────────────────────────────────────────────────────────

// ServiceAccountRow is the DB projection of a service_accounts row.
// Column order is fixed and must match all SELECT queries in this file.
type ServiceAccountRow struct {
	ID          string
	ProjectID   string
	Name        string
	DisplayName string
	Description *string
	Status      string // active | disabled | deleted
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// ── IAMRoleBindingRow ─────────────────────────────────────────────────────────

// IAMRoleBindingRow is the DB projection of an iam_role_bindings row.
// Column order is fixed and must match all SELECT queries in this file.
type IAMRoleBindingRow struct {
	ID           string
	ProjectID    string
	PrincipalID  string
	Role         string // e.g. "roles/owner"
	ResourceType string // e.g. "project", "instance"
	ResourceID   string // FK target within project
	GrantedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ── Error sentinels ───────────────────────────────────────────────────────────

// ErrServiceAccountNotFound is returned when a service account cannot be found
// or is soft-deleted. Callers map this to HTTP 404.
var ErrServiceAccountNotFound = fmt.Errorf("service account not found")

// ErrRoleBindingNotFound is returned when a role binding cannot be found.
var ErrRoleBindingNotFound = fmt.Errorf("iam role binding not found")

// ErrServiceAccountNameConflict is returned when a service account name already
// exists within the project (UNIQUE constraint on (project_id, name)).
var ErrServiceAccountNameConflict = fmt.Errorf("service account name already exists in this project")

// ── IAM Role constants ────────────────────────────────────────────────────────
//
// Canonical role strings stored in iam_role_bindings.role.
// Source: vm-16-01__research__ §"Authorization Model".

const (
	IAMRoleOwner                = "roles/owner"
	IAMRoleComputeViewer        = "roles/compute.viewer"
	IAMRoleComputeInstanceAdmin = "roles/compute.instanceAdmin"
	IAMRoleServiceAccountUser   = "roles/iam.serviceAccountUser"
)

// ── Resource type constants ───────────────────────────────────────────────────

const (
	IAMResourceTypeProject        = "project"
	IAMResourceTypeInstance       = "instance"
	IAMResourceTypeServiceAccount = "service_account"
)

// ── Service Account repo methods ──────────────────────────────────────────────

// CreateServiceAccount inserts a new service_accounts row in 'active' status.
//
// id and created_by are caller-supplied (use idgen.New in the handler layer).
// name must be unique within project_id — a duplicate returns ErrServiceAccountNameConflict.
//
// Source: vm-16-01__blueprint__ §components "IAM Credentials Service" seam,
//
//	vm-16-01__research__ §"Service Account Lifecycle and Credential Model".
func (r *Repo) CreateServiceAccount(
	ctx context.Context,
	id, projectID, name, displayName, createdBy string,
	description *string,
) (*ServiceAccountRow, error) {
	const q = `
INSERT INTO service_accounts (id, project_id, name, display_name, description, status, created_by)
VALUES ($1, $2, $3, $4, $5, 'active', $6)`
	if _, err := r.pool.Exec(ctx, q, id, projectID, name, displayName, description, createdBy); err != nil {
		if isSADuplicateKey(err) {
			return nil, ErrServiceAccountNameConflict
		}
		return nil, fmt.Errorf("CreateServiceAccount: %w", err)
	}
	return r.GetServiceAccountByID(ctx, id, projectID)
}

// GetServiceAccountByID fetches a non-deleted service account by its ID,
// scoped to the given project.
//
// projectID acts as the ownership scope guard — a service account in a
// different project returns ErrServiceAccountNotFound (no existence leak).
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
func (r *Repo) GetServiceAccountByID(ctx context.Context, id, projectID string) (*ServiceAccountRow, error) {
	const q = `
SELECT id, project_id, name, display_name, description, status, created_by, created_at, updated_at, deleted_at
FROM service_accounts
WHERE id = $1 AND project_id = $2 AND deleted_at IS NULL`
	sa, err := scanServiceAccount(r.pool.QueryRow(ctx, q, id, projectID))
	if err != nil {
		if isSANoRows(err) {
			return nil, fmt.Errorf("GetServiceAccountByID %s: %w", id, ErrServiceAccountNotFound)
		}
		return nil, fmt.Errorf("GetServiceAccountByID: %w", err)
	}
	return sa, nil
}

// ListServiceAccountsByProject returns all non-deleted service accounts for a
// project, ordered by creation time ascending.
//
// Source: vm-16-01__research__ §"Service Account Lifecycle and Credential Model".
func (r *Repo) ListServiceAccountsByProject(ctx context.Context, projectID string) ([]*ServiceAccountRow, error) {
	const q = `
SELECT id, project_id, name, display_name, description, status, created_by, created_at, updated_at, deleted_at
FROM service_accounts
WHERE project_id = $1 AND deleted_at IS NULL
ORDER BY created_at ASC`
	rows, err := r.pool.Query(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("ListServiceAccountsByProject: %w", err)
	}
	defer rows.Close()

	var out []*ServiceAccountRow
	for rows.Next() {
		sa := &ServiceAccountRow{}
		if err := rows.Scan(
			&sa.ID, &sa.ProjectID, &sa.Name, &sa.DisplayName,
			&sa.Description, &sa.Status, &sa.CreatedBy,
			&sa.CreatedAt, &sa.UpdatedAt, &sa.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListServiceAccountsByProject scan: %w", err)
		}
		out = append(out, sa)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListServiceAccountsByProject rows err: %w", err)
	}
	return out, nil
}

// SetServiceAccountStatus transitions a service account to "active" or "disabled".
//
// Returns ErrServiceAccountNotFound when the SA does not exist, is not in this
// project, or is already soft-deleted.
//
// Source: vm-16-01__research__ §"Service Account Lifecycle" (ACTIVE, DISABLED states).
func (r *Repo) SetServiceAccountStatus(ctx context.Context, id, projectID, status string) (*ServiceAccountRow, error) {
	const q = `
UPDATE service_accounts
SET status = $3, updated_at = NOW()
WHERE id = $1 AND project_id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, id, projectID, status)
	if err != nil {
		return nil, fmt.Errorf("SetServiceAccountStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("SetServiceAccountStatus %s: %w", id, ErrServiceAccountNotFound)
	}
	return r.GetServiceAccountByID(ctx, id, projectID)
}

// SoftDeleteServiceAccount marks a service account as deleted (sets deleted_at).
//
// Returns ErrServiceAccountNotFound when not found, already deleted, or in a
// different project.
//
// Source: vm-16-01__blueprint__ §implementation_decisions "Project Deletion as Soft-Delete".
func (r *Repo) SoftDeleteServiceAccount(ctx context.Context, id, projectID string) error {
	const q = `
UPDATE service_accounts
SET status = 'deleted', deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND project_id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, id, projectID)
	if err != nil {
		return fmt.Errorf("SoftDeleteServiceAccount: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteServiceAccount %s: %w", id, ErrServiceAccountNotFound)
	}
	return nil
}

// ── IAM Role Binding repo methods ─────────────────────────────────────────────

// CreateRoleBinding inserts a new iam_role_bindings row.
//
// ON CONFLICT DO NOTHING: duplicate bindings (same principal+role+resource)
// are silently ignored; repeated creates are idempotent.
//
// Source: vm-16-01__blueprint__ §core_contracts "Authorization is Hierarchical and Additive".
func (r *Repo) CreateRoleBinding(
	ctx context.Context,
	id, projectID, principalID, role, resourceType, resourceID, grantedBy string,
) (*IAMRoleBindingRow, error) {
	const q = `
INSERT INTO iam_role_bindings (id, project_id, principal_id, role, resource_type, resource_id, granted_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (project_id, principal_id, role, resource_type, resource_id) DO NOTHING`
	if _, err := r.pool.Exec(ctx, q, id, projectID, principalID, role, resourceType, resourceID, grantedBy); err != nil {
		return nil, fmt.Errorf("CreateRoleBinding: %w", err)
	}
	return r.GetRoleBindingByID(ctx, id, projectID)
}

// GetRoleBindingByID fetches a role binding by its ID within a project.
//
// Returns ErrRoleBindingNotFound when absent or in a different project.
func (r *Repo) GetRoleBindingByID(ctx context.Context, id, projectID string) (*IAMRoleBindingRow, error) {
	const q = `
SELECT id, project_id, principal_id, role, resource_type, resource_id, granted_by, created_at, updated_at
FROM iam_role_bindings
WHERE id = $1 AND project_id = $2`
	rb := &IAMRoleBindingRow{}
	err := r.pool.QueryRow(ctx, q, id, projectID).Scan(
		&rb.ID, &rb.ProjectID, &rb.PrincipalID, &rb.Role,
		&rb.ResourceType, &rb.ResourceID, &rb.GrantedBy,
		&rb.CreatedAt, &rb.UpdatedAt,
	)
	if err != nil {
		if isSANoRows(err) {
			return nil, fmt.Errorf("GetRoleBindingByID %s: %w", id, ErrRoleBindingNotFound)
		}
		return nil, fmt.Errorf("GetRoleBindingByID: %w", err)
	}
	return rb, nil
}

// ListRoleBindings returns all role bindings for a project.
// When principalID is non-empty the results are filtered to that principal.
//
// Source: vm-16-01__blueprint__ §components "IAM Policy Service".
func (r *Repo) ListRoleBindings(ctx context.Context, projectID, principalID string) ([]*IAMRoleBindingRow, error) {
	var (
		rows Rows
		err  error
	)
	if principalID != "" {
		const q = `
SELECT id, project_id, principal_id, role, resource_type, resource_id, granted_by, created_at, updated_at
FROM iam_role_bindings
WHERE project_id = $1 AND principal_id = $2
ORDER BY created_at ASC`
		rows, err = r.pool.Query(ctx, q, projectID, principalID)
	} else {
		const q = `
SELECT id, project_id, principal_id, role, resource_type, resource_id, granted_by, created_at, updated_at
FROM iam_role_bindings
WHERE project_id = $1
ORDER BY created_at ASC`
		rows, err = r.pool.Query(ctx, q, projectID)
	}
	if err != nil {
		return nil, fmt.Errorf("ListRoleBindings: %w", err)
	}
	defer rows.Close()

	var out []*IAMRoleBindingRow
	for rows.Next() {
		rb := &IAMRoleBindingRow{}
		if err := rows.Scan(
			&rb.ID, &rb.ProjectID, &rb.PrincipalID, &rb.Role,
			&rb.ResourceType, &rb.ResourceID, &rb.GrantedBy,
			&rb.CreatedAt, &rb.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListRoleBindings scan: %w", err)
		}
		out = append(out, rb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListRoleBindings rows err: %w", err)
	}
	return out, nil
}

// DeleteRoleBinding removes a role binding by ID within a project (hard-delete).
//
// Hard-delete (not soft-delete) because binding removal must take effect
// immediately per the policy-propagation SLO.
//
// Returns ErrRoleBindingNotFound when absent or in a different project.
//
// Source: vm-16-01__blueprint__ §core_contracts "Policy Propagation is Fast and Consistent".
func (r *Repo) DeleteRoleBinding(ctx context.Context, id, projectID string) error {
	const q = `
DELETE FROM iam_role_bindings
WHERE id = $1 AND project_id = $2`
	tag, err := r.pool.Exec(ctx, q, id, projectID)
	if err != nil {
		return fmt.Errorf("DeleteRoleBinding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DeleteRoleBinding %s: %w", id, ErrRoleBindingNotFound)
	}
	return nil
}

// CheckPrincipalHasRole returns true if the given principal has the specified
// role on the given (resource_type, resource_id) pair within the project.
//
// Phase 16A: flat single-row check. Correct for project-level bindings where
// resource_type=IAMResourceTypeProject and resource_id=projectID.
//
// Phase 16B will extend this to hierarchical ancestor traversal using the
// materialized path pattern described in vm-16-01__blueprint__
// §implementation_decisions "Use a Materialized Path for Hierarchy Traversal".
//
// Source: vm-16-01__blueprint__ §core_contracts "Authorization is Hierarchical and Additive",
//
//	AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
func (r *Repo) CheckPrincipalHasRole(
	ctx context.Context,
	projectID, principalID, role, resourceType, resourceID string,
) (bool, error) {
	const q = `
SELECT EXISTS(
  SELECT 1 FROM iam_role_bindings
  WHERE project_id    = $1
    AND principal_id  = $2
    AND role          = $3
    AND resource_type = $4
    AND resource_id   = $5
)`
	var exists bool
	if err := r.pool.QueryRow(ctx, q, projectID, principalID, role, resourceType, resourceID).Scan(&exists); err != nil {
		return false, fmt.Errorf("CheckPrincipalHasRole: %w", err)
	}
	return exists, nil
}

// ── scan helpers ──────────────────────────────────────────────────────────────

func scanServiceAccount(row Row) (*ServiceAccountRow, error) {
	sa := &ServiceAccountRow{}
	if err := row.Scan(
		&sa.ID, &sa.ProjectID, &sa.Name, &sa.DisplayName,
		&sa.Description, &sa.Status, &sa.CreatedBy,
		&sa.CreatedAt, &sa.UpdatedAt, &sa.DeletedAt,
	); err != nil {
		return nil, err
	}
	return sa, nil
}

// ── file-local error helpers ──────────────────────────────────────────────────
//
// Prefixed "isSA" to avoid redeclaration with the package-level helpers that
// exist in an unuploaded db file (quota_repo.go or helpers.go).

// isSANoRows detects the "no rows" error returned by the Pool layer.
// Matches the string returned by both pgx ("no rows in result set") and the
// memPool / fakePool in tests (fmt.Errorf("no rows in result set")).
// Source pattern: db.go GetHostByID and GetCampaignByID inline check.
func isSANoRows(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "no rows in result set"
}

// isSADuplicateKey detects PostgreSQL unique-constraint violations.
// lib/pq surfaces these as plain error messages containing "duplicate key".
func isSADuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate key")
}
