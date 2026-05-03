package db

// project_repo.go — Project persistence for VM-P2D foundation slice.
//
// Replaces internal/db/project_compile_stub.go.
// All SQL patterns match the memPool dispatch in
// services/resource-manager/instance_handlers_test.go exactly.
//
// Source: P2_PROJECT_RBAC_MODEL.md §2.4 (schema), §9 (API endpoints),
//         AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account),
//         API_ERROR_CONTRACT_V1 §4 (error codes).

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProjectRow is the DB projection of a projects row.
//
// Column order is fixed and must match all SELECT queries in this file and
// the projRow/projRows.Scan implementations in instance_handlers_test.go:
//
//	0: id           *string
//	1: principal_id *string
//	2: created_by   *string
//	3: name         *string
//	4: display_name *string
//	5: description  **string
//	6: status       *string
//	7: created_at   *time.Time
//	8: updated_at   *time.Time
//	9: deleted_at   **time.Time
type ProjectRow struct {
	ID          string
	PrincipalID string
	CreatedBy   string
	Name        string
	DisplayName string
	Description *string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// CreateProject inserts a principals row (principal_type='PROJECT') then a
// projects row, and returns the freshly fetched project.
//
// SQL patterns match memPool.Exec dispatch:
//
//	"INSERT INTO principals" → $1=id, $2=principal_type
//	"INSERT INTO projects"   → $1=id, $2=principal_id, $3=created_by,
//	                           $4=name, $5=display_name, $6=description, $7=status
func (r *Repo) CreateProject(
	ctx context.Context,
	principalID, projectID, createdBy, name, displayName string,
	description *string,
) (*ProjectRow, error) {
	const qPrincipal = `INSERT INTO principals (id, principal_type) VALUES ($1, $2)`
	if _, err := r.pool.Exec(ctx, qPrincipal, principalID, "PROJECT"); err != nil {
		return nil, fmt.Errorf("create project principal: %w", err)
	}

	const qProject = `
INSERT INTO projects (id, principal_id, created_by, name, display_name, description, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)`
	if _, err := r.pool.Exec(ctx, qProject,
		projectID, principalID, createdBy, name, displayName, description, "active",
	); err != nil {
		return nil, fmt.Errorf("create project row: %w", err)
	}

	return r.GetProjectByID(ctx, projectID)
}

// GetProjectByID fetches a non-deleted project by its ID.
// Returns an error wrapping sql.ErrNoRows when not found or soft-deleted.
//
// SQL pattern matches memPool.QueryRow dispatch:
//
//	"FROM projects" AND "id = $1"
func (r *Repo) GetProjectByID(ctx context.Context, id string) (*ProjectRow, error) {
	const q = `
SELECT id, principal_id, created_by, name, display_name, description,
       status, created_at, updated_at, deleted_at
FROM projects
WHERE id = $1 AND deleted_at IS NULL`
	row := r.pool.QueryRow(ctx, q, id)
	return scanProject(row)
}

// GetProjectByPrincipalID fetches a non-deleted project by its principal_id.
//
// SQL pattern matches memPool.QueryRow dispatch:
//
//	"FROM projects" AND "principal_id = $1"
func (r *Repo) GetProjectByPrincipalID(ctx context.Context, principalID string) (*ProjectRow, error) {
	const q = `
SELECT id, principal_id, created_by, name, display_name, description,
       status, created_at, updated_at, deleted_at
FROM projects
WHERE principal_id = $1 AND deleted_at IS NULL`
	row := r.pool.QueryRow(ctx, q, principalID)
	return scanProject(row)
}

// CheckProjectNameExists returns true if a non-deleted project with the given
// (createdBy, name) pair exists, excluding the project with excludeID.
// Pass excludeID="" to exclude nothing (used on create).
//
// SQL pattern matches memPool.QueryRow dispatch:
//
//	"SELECT EXISTS" AND "FROM projects" → $1=created_by, $2=name, $3=excludeID
func (r *Repo) CheckProjectNameExists(ctx context.Context, createdBy, name, excludeID string) (bool, error) {
	const q = `
SELECT EXISTS(
    SELECT 1 FROM projects
    WHERE created_by = $1
      AND name = $2
      AND id != $3
      AND deleted_at IS NULL
)`
	row := r.pool.QueryRow(ctx, q, createdBy, name, excludeID)
	var exists bool
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("check project name exists: %w", err)
	}
	return exists, nil
}

// ListProjectsByCreator returns all non-deleted projects created by the given principal,
// ordered by creation time ascending.
//
// SQL pattern matches memPool.Query dispatch:
//
//	"FROM projects" AND "created_by"
func (r *Repo) ListProjectsByCreator(ctx context.Context, createdBy string) ([]*ProjectRow, error) {
	const q = `
SELECT id, principal_id, created_by, name, display_name, description,
       status, created_at, updated_at, deleted_at
FROM projects
WHERE created_by = $1 AND deleted_at IS NULL
ORDER BY created_at ASC`
	rows, err := r.pool.Query(ctx, q, createdBy)
	if err != nil {
		return nil, fmt.Errorf("list projects by creator: %w", err)
	}
	defer rows.Close()

	var out []*ProjectRow
	for rows.Next() {
		p := &ProjectRow{}
		if err := rows.Scan(
			&p.ID, &p.PrincipalID, &p.CreatedBy, &p.Name, &p.DisplayName,
			&p.Description, &p.Status, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("list projects scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects rows err: %w", err)
	}
	return out, nil
}

// UpdateProject updates the mutable fields (name, display_name, description) of a
// non-deleted project, then returns the updated row.
// Returns an error wrapping sql.ErrNoRows if the project is not found or already deleted.
//
// SQL pattern matches memPool.Exec dispatch:
//
//	"UPDATE projects" AND "name = $2" → $1=id, $2=name, $3=display_name, $4=description
func (r *Repo) UpdateProject(ctx context.Context, id, name, displayName string, description *string) (*ProjectRow, error) {
	const q = `
UPDATE projects
SET name = $2, display_name = $3, description = $4, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, id, name, displayName, description)
	if err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("update project %s: %w", id, sql.ErrNoRows)
	}
	return r.GetProjectByID(ctx, id)
}

// SoftDeleteProject marks the project deleted by setting deleted_at and status='deleted'.
// Returns an error wrapping sql.ErrNoRows if not found or already deleted.
//
// SQL pattern matches memPool.Exec dispatch:
//
//	"UPDATE projects" AND "SET deleted_at = NOW()" → $1=id
func (r *Repo) SoftDeleteProject(ctx context.Context, id string) error {
	const q = `
UPDATE projects
SET deleted_at = NOW(), status = 'deleted', updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("soft delete project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("soft delete project %s: %w", id, sql.ErrNoRows)
	}
	return nil
}

// ── Project membership ───────────────────────────────────────────────────────

// ProjectMemberRow is the DB projection of a project_members row.
//
// Column order matches all SELECT queries:
//
//	0: project_id   string
//	1: principal_id string
//	2: role         string
//	3: added_by     string
//	4: created_at   time.Time
//	5: updated_at   time.Time
type ProjectMemberRow struct {
	ProjectID   string
	PrincipalID string
	Role        string
	AddedBy     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// IAM role constants for project membership.
const (
	ProjectRoleOwner    = "roles/owner"
	ProjectRoleAdmin    = "roles/admin"
	ProjectRoleOperator = "roles/operator"
	ProjectRoleViewer   = "roles/viewer"
)

// AddProjectMember inserts a row into project_members.
// Returns (nil, nil) on duplicate key (member already exists).
func (r *Repo) AddProjectMember(ctx context.Context, projectID, principalID, role, addedBy string) (*ProjectMemberRow, error) {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO project_members (project_id, principal_id, role, added_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (project_id, principal_id) DO NOTHING
	`, projectID, principalID, role, addedBy)
	if err != nil {
		return nil, fmt.Errorf("AddProjectMember: %w", err)
	}
	return r.GetProjectMember(ctx, projectID, principalID)
}

// GetProjectMember fetches a single membership row.
// Returns nil, nil when not found (caller checks nil).
func (r *Repo) GetProjectMember(ctx context.Context, projectID, principalID string) (*ProjectMemberRow, error) {
	m := &ProjectMemberRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT project_id, principal_id, role, added_by, created_at, updated_at
		FROM project_members
		WHERE project_id = $1 AND principal_id = $2
	`, projectID, principalID).Scan(
		&m.ProjectID, &m.PrincipalID, &m.Role, &m.AddedBy, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetProjectMember: %w", err)
	}
	return m, nil
}

// ListProjectMembers returns all members of a project.
func (r *Repo) ListProjectMembers(ctx context.Context, projectID string) ([]*ProjectMemberRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT project_id, principal_id, role, added_by, created_at, updated_at
		FROM project_members
		WHERE project_id = $1
		ORDER BY created_at ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("ListProjectMembers: %w", err)
	}
	defer rows.Close()

	var out []*ProjectMemberRow
	for rows.Next() {
		m := &ProjectMemberRow{}
		if err := rows.Scan(
			&m.ProjectID, &m.PrincipalID, &m.Role, &m.AddedBy, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListProjectMembers scan: %w", err)
		}
		out = append(out, m)
	}
	if out == nil {
		out = []*ProjectMemberRow{}
	}
	return out, rows.Err()
}

// RemoveProjectMember deletes a membership row.
// Returns nil when no row matched (member not present).
func (r *Repo) RemoveProjectMember(ctx context.Context, projectID, principalID string) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM project_members
		WHERE project_id = $1 AND principal_id = $2
	`, projectID, principalID)
	if err != nil {
		return fmt.Errorf("RemoveProjectMember: %w", err)
	}
	return nil
}

// CheckProjectMemberHasRole returns true when the principal has at least the
// given role in the project. Role hierarchy: owner > admin > operator > viewer.
// An owner also satisfies admin/operator/viewer checks. Admin also
// satisfies operator/viewer. Operator also satisfies viewer.
func (r *Repo) CheckProjectMemberHasRole(ctx context.Context, projectID, principalID, minRole string) (bool, error) {
	m, err := r.GetProjectMember(ctx, projectID, principalID)
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, nil
	}
	return roleIsAtLeast(m.Role, minRole), nil
}

// roleIsAtLeast returns true when actualRole meets or exceeds minRole in the
// hierarchy: owner(4) > admin(3) > operator(2) > viewer(1).
func roleIsAtLeast(actualRole, minRole string) bool {
	return roleLevel(actualRole) >= roleLevel(minRole)
}

func roleLevel(r string) int {
	switch r {
	case ProjectRoleOwner:
		return 4
	case ProjectRoleAdmin:
		return 3
	case ProjectRoleOperator:
		return 2
	case ProjectRoleViewer:
		return 1
	default:
		return 0
	}
}

// scanProject scans a single db.Row into a ProjectRow.
// Column order must match the SELECT lists in GetProjectByID and GetProjectByPrincipalID.
func scanProject(row Row) (*ProjectRow, error) {
	p := &ProjectRow{}
	if err := row.Scan(
		&p.ID, &p.PrincipalID, &p.CreatedBy, &p.Name, &p.DisplayName,
		&p.Description, &p.Status, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt,
	); err != nil {
		return nil, err
	}
	return p, nil
}
