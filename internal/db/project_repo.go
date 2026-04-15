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
