package main

// iam_handlers.go — Service Account and IAM Role Binding HTTP handlers.
//
// Phase 16A: Smallest correct repo-native IAM seams.
//
// Endpoints registered by registerIAMRoutes:
//
//   Service Accounts (scoped to a project):
//     POST   /v1/projects/{project_id}/service-accounts            → 201
//     GET    /v1/projects/{project_id}/service-accounts            → 200
//     GET    /v1/projects/{project_id}/service-accounts/{sa_id}    → 200
//     POST   /v1/projects/{project_id}/service-accounts/{sa_id}/disable  → 200
//     POST   /v1/projects/{project_id}/service-accounts/{sa_id}/enable   → 200
//     DELETE /v1/projects/{project_id}/service-accounts/{sa_id}    → 204
//
//   IAM Role Bindings (scoped to a project):
//     POST   /v1/projects/{project_id}/iam/bindings                → 201
//     GET    /v1/projects/{project_id}/iam/bindings                → 200
//     GET    /v1/projects/{project_id}/iam/bindings/{binding_id}   → 200
//     DELETE /v1/projects/{project_id}/iam/bindings/{binding_id}   → 204
//
// Auth: X-Principal-ID header (same pattern as instance and project handlers).
// Cross-project access always returns 404 (not 403).
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
//
// IAM Credentials Service (token vending / impersonation) is deferred to
// Phase 16B per vm-16-01__blueprint__ §mvp §deferred.
//
// Source: vm-16-01__blueprint__ §components,
//         vm-16-01__research__ §"Service Account Lifecycle and Credential Model",
//         AUTH_OWNERSHIP_MODEL_V1 §3,
//         API_ERROR_CONTRACT_V1 §1–§4.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── Request / Response types ──────────────────────────────────────────────────

type createServiceAccountRequest struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Description *string `json:"description,omitempty"`
}

type serviceAccountResponse struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"project_id"`
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Description *string `json:"description,omitempty"`
	Status      string  `json:"status"`
	CreatedBy   string  `json:"created_by"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func saToResponse(sa *db.ServiceAccountRow) serviceAccountResponse {
	return serviceAccountResponse{
		ID:          sa.ID,
		ProjectID:   sa.ProjectID,
		Name:        sa.Name,
		DisplayName: sa.DisplayName,
		Description: sa.Description,
		Status:      sa.Status,
		CreatedBy:   sa.CreatedBy,
		CreatedAt:   sa.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   sa.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type createRoleBindingRequest struct {
	PrincipalID  string `json:"principal_id"`
	Role         string `json:"role"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

type roleBindingResponse struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	PrincipalID  string `json:"principal_id"`
	Role         string `json:"role"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	GrantedBy    string `json:"granted_by"`
	CreatedAt    string `json:"created_at"`
}

func rbToResponse(rb *db.IAMRoleBindingRow) roleBindingResponse {
	return roleBindingResponse{
		ID:           rb.ID,
		ProjectID:    rb.ProjectID,
		PrincipalID:  rb.PrincipalID,
		Role:         rb.Role,
		ResourceType: rb.ResourceType,
		ResourceID:   rb.ResourceID,
		GrantedBy:    rb.GrantedBy,
		CreatedAt:    rb.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ── Route registration ────────────────────────────────────────────────────────

// registerIAMRoutes wires IAM endpoints into mux.
// All routes are project-scoped under /v1/projects/{project_id}/.
func (s *server) registerIAMRoutes(mux *http.ServeMux) {
	// No direct mux registration here.
	// IAM project subpaths are dispatched from the existing
	// project handler in project_handlers.go.
	_ = mux
}

// handleIAMSubpath dispatches all /v1/projects/{id}/service-accounts[/...]
// and /v1/projects/{id}/iam/bindings[/...] requests.
//
// This handler is registered on /v1/projects/ which is already claimed by the
// project handler.  To avoid double-registration we integrate into the existing
// handleProjectByID dispatch via handleIAMSubpath, called from there.
//
// NOTE: registerIAMRoutes does NOT re-register /v1/projects/ — instead
// handleProjectByID in project_handlers.go is patched to call
// s.handleIAMSubpath when it sees a recognized IAM sub-path segment.
// The registration here is a no-op placeholder kept for documentation;
// actual dispatch is via the patched project_handlers.go.
//
// See: api.go registerIAMRoutes note and project_handlers.go handleProjectByID.

// handleIAMSubpath is called by handleProjectByID when the path beyond the
// project ID segment is a recognised IAM sub-path.
//
// path is the tail after /v1/projects/{project_id}/, e.g.:
//   "service-accounts"
//   "service-accounts/sa_abc123"
//   "service-accounts/sa_abc123/disable"
//   "iam/bindings"
//   "iam/bindings/rb_abc123"
//
// principalID is the X-Principal-ID header value (already validated by caller).
// projectID is already extracted by the caller.
func (s *server) handleIAMSubpath(w http.ResponseWriter, r *http.Request, projectID, principalID, path string) {
	switch {
	case path == "service-accounts":
		s.handleServiceAccountsCollection(w, r, projectID, principalID)

	case strings.HasPrefix(path, "service-accounts/"):
		rest := strings.TrimPrefix(path, "service-accounts/")
		parts := strings.SplitN(rest, "/", 2)
		saID := parts[0]
		if saID == "" {
			writeAPIError(w, http.StatusNotFound, errServiceAccountNotFound, "Service account not found.", "")
			return
		}
		if len(parts) == 2 {
			s.handleServiceAccountAction(w, r, projectID, principalID, saID, parts[1])
		} else {
			s.handleServiceAccountByID(w, r, projectID, principalID, saID)
		}

	case path == "iam/bindings":
		s.handleRoleBindingsCollection(w, r, projectID, principalID)

	case strings.HasPrefix(path, "iam/bindings/"):
		bindingID := strings.TrimPrefix(path, "iam/bindings/")
		if bindingID == "" || strings.Contains(bindingID, "/") {
			writeAPIError(w, http.StatusNotFound, errRoleBindingNotFound, "Role binding not found.", "")
			return
		}
		s.handleRoleBindingByID(w, r, projectID, principalID, bindingID)

	default:
		http.NotFound(w, r)
	}
}

// ── Service Account handlers ──────────────────────────────────────────────────

// handleServiceAccountsCollection dispatches POST / GET on
// /v1/projects/{project_id}/service-accounts.
func (s *server) handleServiceAccountsCollection(w http.ResponseWriter, r *http.Request, projectID, principalID string) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateServiceAccount(w, r, projectID, principalID)
	case http.MethodGet:
		s.handleListServiceAccounts(w, r, projectID, principalID)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "Method not allowed.", "")
	}
}

// handleServiceAccountByID dispatches GET / DELETE on
// /v1/projects/{project_id}/service-accounts/{sa_id}.
func (s *server) handleServiceAccountByID(w http.ResponseWriter, r *http.Request, projectID, principalID, saID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetServiceAccount(w, r, projectID, principalID, saID)
	case http.MethodDelete:
		s.handleDeleteServiceAccount(w, r, projectID, principalID, saID)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "Method not allowed.", "")
	}
}

// handleServiceAccountAction dispatches POST on
// /v1/projects/{project_id}/service-accounts/{sa_id}/{action}.
// action: "disable" | "enable"
func (s *server) handleServiceAccountAction(w http.ResponseWriter, r *http.Request, projectID, principalID, saID, action string) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "Method not allowed.", "")
		return
	}
	switch action {
	case "disable":
		s.handleSetServiceAccountStatus(w, r, projectID, principalID, saID, "disabled")
	case "enable":
		s.handleSetServiceAccountStatus(w, r, projectID, principalID, saID, "active")
	default:
		http.NotFound(w, r)
	}
}

// handleCreateServiceAccount handles POST /v1/projects/{project_id}/service-accounts.
// Returns 201 Created + serviceAccountResponse.
func (s *server) handleCreateServiceAccount(w http.ResponseWriter, r *http.Request, projectID, principalID string) {
	// Verify the project exists and is owned by the caller.
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	var req createServiceAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest, "Invalid JSON: "+err.Error(), "")
		return
	}

	if errs := validateServiceAccountCreate(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	saID := idgen.New("sa")
	sa, err := s.repo.CreateServiceAccount(r.Context(),
		saID, projectID, req.Name, req.DisplayName, principalID, req.Description)
	if err != nil {
		if errors.Is(err, db.ErrServiceAccountNameConflict) {
			writeAPIError(w, http.StatusConflict, errInvalidRequest,
				"A service account named '"+req.Name+"' already exists in this project.", "name")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusCreated, saToResponse(sa))
}

// handleListServiceAccounts handles GET /v1/projects/{project_id}/service-accounts.
func (s *server) handleListServiceAccounts(w http.ResponseWriter, r *http.Request, projectID, principalID string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	sas, err := s.repo.ListServiceAccountsByProject(r.Context(), projectID)
	if err != nil {
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	out := make([]serviceAccountResponse, 0, len(sas))
	for _, sa := range sas {
		out = append(out, saToResponse(sa))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"service_accounts": out})
}

// handleGetServiceAccount handles GET /v1/projects/{project_id}/service-accounts/{sa_id}.
func (s *server) handleGetServiceAccount(w http.ResponseWriter, r *http.Request, projectID, principalID, saID string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	sa, err := s.repo.GetServiceAccountByID(r.Context(), saID, projectID)
	if err != nil {
		if isServiceAccountNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errServiceAccountNotFound, "Service account not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, saToResponse(sa))
}

// handleSetServiceAccountStatus handles POST .../disable and .../enable.
func (s *server) handleSetServiceAccountStatus(w http.ResponseWriter, r *http.Request, projectID, principalID, saID, newStatus string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	sa, err := s.repo.SetServiceAccountStatus(r.Context(), saID, projectID, newStatus)
	if err != nil {
		if isServiceAccountNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errServiceAccountNotFound, "Service account not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, saToResponse(sa))
}

// handleDeleteServiceAccount handles DELETE /v1/projects/{project_id}/service-accounts/{sa_id}.
// Returns 204 No Content on success.
func (s *server) handleDeleteServiceAccount(w http.ResponseWriter, r *http.Request, projectID, principalID, saID string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	if err := s.repo.SoftDeleteServiceAccount(r.Context(), saID, projectID); err != nil {
		if isServiceAccountNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errServiceAccountNotFound, "Service account not found.", "")
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

// ── Role Binding handlers ─────────────────────────────────────────────────────

// handleRoleBindingsCollection dispatches POST / GET on
// /v1/projects/{project_id}/iam/bindings.
func (s *server) handleRoleBindingsCollection(w http.ResponseWriter, r *http.Request, projectID, principalID string) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateRoleBinding(w, r, projectID, principalID)
	case http.MethodGet:
		s.handleListRoleBindings(w, r, projectID, principalID)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "Method not allowed.", "")
	}
}

// handleRoleBindingByID dispatches GET / DELETE on
// /v1/projects/{project_id}/iam/bindings/{binding_id}.
func (s *server) handleRoleBindingByID(w http.ResponseWriter, r *http.Request, projectID, principalID, bindingID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetRoleBinding(w, r, projectID, principalID, bindingID)
	case http.MethodDelete:
		s.handleDeleteRoleBinding(w, r, projectID, principalID, bindingID)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, errInvalidRequest, "Method not allowed.", "")
	}
}

// handleCreateRoleBinding handles POST /v1/projects/{project_id}/iam/bindings.
// Returns 201 Created + roleBindingResponse.
func (s *server) handleCreateRoleBinding(w http.ResponseWriter, r *http.Request, projectID, principalID string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	var req createRoleBindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest, "Invalid JSON: "+err.Error(), "")
		return
	}

	if errs := validateRoleBindingCreate(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	bindingID := idgen.New("rb")
	rb, err := s.repo.CreateRoleBinding(r.Context(),
		bindingID, projectID, req.PrincipalID, req.Role,
		req.ResourceType, req.ResourceID, principalID)
	if err != nil {
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}
	if rb == nil {
		// ON CONFLICT DO NOTHING returned 0 rows — binding already exists.
		writeAPIError(w, http.StatusConflict, errRoleBindingConflict,
			"This role binding already exists.", "")
		return
	}

	writeJSON(w, http.StatusCreated, rbToResponse(rb))
}

// handleListRoleBindings handles GET /v1/projects/{project_id}/iam/bindings.
// Optional ?principal_id= query parameter filters by principal.
func (s *server) handleListRoleBindings(w http.ResponseWriter, r *http.Request, projectID, principalID string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	filterPrincipal := r.URL.Query().Get("principal_id")

	rbs, err := s.repo.ListRoleBindings(r.Context(), projectID, filterPrincipal)
	if err != nil {
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	out := make([]roleBindingResponse, 0, len(rbs))
	for _, rb := range rbs {
		out = append(out, rbToResponse(rb))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"bindings": out})
}

// handleGetRoleBinding handles GET /v1/projects/{project_id}/iam/bindings/{id}.
func (s *server) handleGetRoleBinding(w http.ResponseWriter, r *http.Request, projectID, principalID, bindingID string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	rb, err := s.repo.GetRoleBindingByID(r.Context(), bindingID, projectID)
	if err != nil {
		if isRoleBindingNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errRoleBindingNotFound, "Role binding not found.", "")
			return
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return
		}
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, rbToResponse(rb))
}

// handleDeleteRoleBinding handles DELETE /v1/projects/{project_id}/iam/bindings/{id}.
// Returns 204 No Content on success.
// Hard-delete: revocations take effect immediately.
func (s *server) handleDeleteRoleBinding(w http.ResponseWriter, r *http.Request, projectID, principalID, bindingID string) {
	if ok := s.requireProjectOwnership(w, r, projectID, principalID); !ok {
		return
	}

	if err := s.repo.DeleteRoleBinding(r.Context(), bindingID, projectID); err != nil {
		if isRoleBindingNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errRoleBindingNotFound, "Role binding not found.", "")
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

// ── Validation ────────────────────────────────────────────────────────────────

func validateServiceAccountCreate(req *createServiceAccountRequest) []fieldErr {
	var errs []fieldErr
	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fieldErr{errMissingField, "name is required.", "name"})
	} else if len(req.Name) > 63 {
		errs = append(errs, fieldErr{errInvalidValue, "name must be 63 characters or fewer.", "name"})
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		errs = append(errs, fieldErr{errMissingField, "display_name is required.", "display_name"})
	}
	return errs
}

func validateRoleBindingCreate(req *createRoleBindingRequest) []fieldErr {
	var errs []fieldErr
	if strings.TrimSpace(req.PrincipalID) == "" {
		errs = append(errs, fieldErr{errMissingField, "principal_id is required.", "principal_id"})
	}
	if strings.TrimSpace(req.Role) == "" {
		errs = append(errs, fieldErr{errMissingField, "role is required.", "role"})
	}
	if strings.TrimSpace(req.ResourceType) == "" {
		errs = append(errs, fieldErr{errMissingField, "resource_type is required.", "resource_type"})
	}
	if strings.TrimSpace(req.ResourceID) == "" {
		errs = append(errs, fieldErr{errMissingField, "resource_id is required.", "resource_id"})
	}
	return errs
}

// ── Shared project-ownership guard ────────────────────────────────────────────

// requireProjectOwnership fetches the project and verifies the caller owns it.
// Returns true when the check passes. On failure it writes the appropriate error
// and returns false — the caller must return immediately.
//
// Uses the same 404-for-cross-account rule as project_handlers.go.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) requireProjectOwnership(w http.ResponseWriter, r *http.Request, projectID, principalID string) bool {
	proj, err := s.repo.GetProjectByID(r.Context(), projectID)
	if err != nil {
		if isProjectNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errProjectNotFound, "Project not found.", "project_id")
			return false
		}
		if isDBUnavailableError(err) {
			writeServiceUnavailable(w)
			return false
		}
		writeInternalError(w)
		return false
	}
	// Cross-account: hide existence per AUTH_OWNERSHIP_MODEL_V1 §3.
	if proj.CreatedBy != principalID {
		writeAPIError(w, http.StatusNotFound, errProjectNotFound, "Project not found.", "project_id")
		return false
	}
	return true
}

// ── not-found detection helpers ───────────────────────────────────────────────

// isServiceAccountNotFound returns true when err indicates a missing or
// soft-deleted service account.
func isServiceAccountNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, db.ErrServiceAccountNotFound) ||
		strings.Contains(err.Error(), "service account not found")
}

// isRoleBindingNotFound returns true when err indicates a missing role binding.
func isRoleBindingNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, db.ErrRoleBindingNotFound) ||
		strings.Contains(err.Error(), "iam role binding not found")
}
