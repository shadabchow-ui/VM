package main

// project_handlers.go — Project API handlers for VM-P2D foundation slice.
//
// Replaces services/resource-manager/project_routes_test_stub_test.go.
//
// Endpoints registered by registerProjectRoutes:
//   POST   /v1/projects          → handleCreateProject   (201 Created)
//   GET    /v1/projects          → handleListProjects    (200 OK)
//   GET    /v1/projects/{id}     → handleGetProject      (200 OK)
//   PATCH  /v1/projects/{id}     → handleUpdateProject   (200 OK)
//   DELETE /v1/projects/{id}     → handleDeleteProject   (204 No Content)
//
// Auth: X-Principal-ID header (same pattern as instance handlers).
// Cross-account access always returns 404. Source: AUTH_OWNERSHIP_MODEL_V1 §3.
// Error envelope: writeAPIError / writeAPIErrors. Source: API_ERROR_CONTRACT_V1 §1.
//
// Member management (GET/POST/PATCH/DELETE /v1/projects/{id}/members) is
// deferred; not in this slice per VM-P2D scope.
//
// Source: P2_PROJECT_RBAC_MODEL.md §9, AUTH_OWNERSHIP_MODEL_V1 §3,
//         API_ERROR_CONTRACT_V1 §1–§4, P2_MIGRATION_COMPATIBILITY_RULES.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── Request / Response types ──────────────────────────────────────────────────

type createProjectRequest struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Description *string `json:"description,omitempty"`
}

type updateProjectRequest struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Description *string `json:"description,omitempty"`
}

type projectResponse struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Description *string `json:"description,omitempty"`
	CreatedBy   string  `json:"created_by"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func projectToResponse(p *db.ProjectRow) projectResponse {
	return projectResponse{
		ID:          p.ID,
		Name:        p.Name,
		DisplayName: p.DisplayName,
		Description: p.Description,
		CreatedBy:   p.CreatedBy,
		Status:      p.Status,
		CreatedAt:   p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ── Route registration ────────────────────────────────────────────────────────

// registerProjectRoutes wires project management endpoints into mux.
// Replaces the no-op stub in project_routes_test_stub_test.go.
func (s *server) registerProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/projects", s.handleProjects)
	mux.HandleFunc("/v1/projects/", s.handleProjectByID)
}

// handleProjects dispatches POST and GET on /v1/projects.
func (s *server) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateProject(w, r)
	case http.MethodGet:
		s.handleListProjects(w, r)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "Method not allowed.", "")
	}
}

// handleProjectByID dispatches GET, PATCH, DELETE on /v1/projects/{id}.
// Sub-paths (e.g. /v1/projects/{id}/members) are rejected with 404 until
// member management is implemented in a later slice.
func (s *server) handleProjectByID(w http.ResponseWriter, r *http.Request) {
	// Strip prefix and isolate the id segment.
	// Path pattern: /v1/projects/{id}   (no further segments in this slice)
	tail := strings.TrimPrefix(r.URL.Path, "/v1/projects/")
	if tail == "" || strings.Contains(tail, "/") {
		// Either empty id or a sub-path not yet implemented.
		writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
		return
	}
	id := tail

	switch r.Method {
	case http.MethodGet:
		s.handleGetProject(w, r, id)
	case http.MethodPatch:
		s.handleUpdateProject(w, r, id)
	case http.MethodDelete:
		s.handleDeleteProject(w, r, id)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "Method not allowed.", "")
	}
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// handleCreateProject handles POST /v1/projects.
// Returns 201 Created + projectResponse on success.
func (s *server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-ID")
	if principalID == "" {
		writeAPIError(w, http.StatusUnauthorized, errAuthRequired, "Authentication required.", "")
		return
	}

	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest, "Invalid JSON: "+err.Error(), "")
		return
	}

	if errs := validateProjectCreate(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	// Name uniqueness check within this account.
	// excludeID="" means exclude nothing (no existing project to skip).
	exists, err := s.repo.CheckProjectNameExists(r.Context(), principalID, req.Name, "")
	if err != nil {
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}
	if exists {
		writeAPIError(w, http.StatusConflict, errInvalidRequest,
			"A project named '"+req.Name+"' already exists.", "name")
		return
	}

	// Generate IDs. principalRowID is the project's entry in the principals table;
	// projectID is the project's own primary key.
	// Source: P2_PROJECT_RBAC_MODEL.md §2.2 (id format: proj_ prefix).
	principalRowID := idgen.New("prin")
	projectID := idgen.New("proj")

	proj, err := s.repo.CreateProject(r.Context(),
		principalRowID, projectID, principalID, req.Name, req.DisplayName, req.Description)
	if err != nil {
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusCreated, projectToResponse(proj))
}

// handleListProjects handles GET /v1/projects.
// Returns all non-deleted projects created by the requesting principal.
func (s *server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-ID")
	if principalID == "" {
		writeAPIError(w, http.StatusUnauthorized, errAuthRequired, "Authentication required.", "")
		return
	}

	projects, err := s.repo.ListProjectsByCreator(r.Context(), principalID)
	if err != nil {
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	out := make([]projectResponse, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectToResponse(p))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"projects": out})
}

// handleGetProject handles GET /v1/projects/{id}.
// Returns 404 for cross-account access per AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) handleGetProject(w http.ResponseWriter, r *http.Request, id string) {
	principalID := r.Header.Get("X-Principal-ID")
	if principalID == "" {
		writeAPIError(w, http.StatusUnauthorized, errAuthRequired, "Authentication required.", "")
		return
	}

	proj, err := s.repo.GetProjectByID(r.Context(), id)
	if err != nil {
		if isProjectNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	// Cross-account access → 404, never 403. Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	if proj.CreatedBy != principalID {
		writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
		return
	}

	writeJSON(w, http.StatusOK, projectToResponse(proj))
}

// handleUpdateProject handles PATCH /v1/projects/{id}.
// Ownership is verified before the body is decoded.
func (s *server) handleUpdateProject(w http.ResponseWriter, r *http.Request, id string) {
	principalID := r.Header.Get("X-Principal-ID")
	if principalID == "" {
		writeAPIError(w, http.StatusUnauthorized, errAuthRequired, "Authentication required.", "")
		return
	}

	// Ownership check before reading body — same order as instance handlers.
	proj, err := s.repo.GetProjectByID(r.Context(), id)
	if err != nil {
		if isProjectNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}
	if proj.CreatedBy != principalID {
		writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
		return
	}

	var req updateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest, "Invalid JSON: "+err.Error(), "")
		return
	}

	if errs := validateProjectUpdate(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	// Name uniqueness check, excluding this project (so renaming to same name is a no-op).
	exists, err := s.repo.CheckProjectNameExists(r.Context(), principalID, req.Name, id)
	if err != nil {
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}
	if exists {
		writeAPIError(w, http.StatusConflict, errInvalidRequest,
			"A project named '"+req.Name+"' already exists.", "name")
		return
	}

	updated, err := s.repo.UpdateProject(r.Context(), id, req.Name, req.DisplayName, req.Description)
	if err != nil {
		if isProjectNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, projectToResponse(updated))
}

// handleDeleteProject handles DELETE /v1/projects/{id}.
// Ownership is verified before deletion. Returns 204 No Content on success.
func (s *server) handleDeleteProject(w http.ResponseWriter, r *http.Request, id string) {
	principalID := r.Header.Get("X-Principal-ID")
	if principalID == "" {
		writeAPIError(w, http.StatusUnauthorized, errAuthRequired, "Authentication required.", "")
		return
	}

	proj, err := s.repo.GetProjectByID(r.Context(), id)
	if err != nil {
		if isProjectNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}
	if proj.CreatedBy != principalID {
		writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
		return
	}

	if err := s.repo.SoftDeleteProject(r.Context(), id); err != nil {
		if isProjectNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound, "Project not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// isProjectNotFound returns true when err represents a missing or soft-deleted project.
// Covers: sql.ErrNoRows (pgx), "no rows in result set" (memPool in tests),
// and the wrapped fmt.Errorf("... %w", sql.ErrNoRows) form used by project_repo.go.
func isProjectNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
