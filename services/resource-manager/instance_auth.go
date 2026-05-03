package main

// instance_auth.go — Auth middleware and ownership helpers for the public instance API.
//
// PASS 2 scope:
//   - requirePrincipal: middleware enforcing X-Principal-ID header presence.
//   - principalFromCtx: extracts authenticated principal from request context.
//   - loadOwnedInstance: fetches instance and enforces ownership, returns 404 on any miss.
//
// VM-ADMISSION-SCHEDULER-RBAC-PHASE-G-H additions:
//   - loadOwnedImage: centralized image ownership enforcement (mirrors loadOwnedInstance/loadOwnedVolume).
//   - CheckProjectAccess: RBAC helper that verifies the caller has a project role
//     (or owns the resource directly in classic/no-project mode).
//   - checkProjectRole: internal helper for project role lookup with 404-for-cross-account.
//
// P2-M1/WS-H1: loadOwnedInstance now calls writeDBError (not writeInternalError directly)
//   so transient PostgreSQL connectivity failures return 503 instead of 500.
//   Gate item DB-6.
//
// Auth model (M5-level):
//   The edge gateway (future SigV4 layer) sets X-Principal-ID after verifying the
//   request signature. The resource-manager trusts this header on the internal network.
//   Full SigV4 + KMS verification is post-M5 work (requires Linux hardware + api_keys table).
//
// Ownership rule: if the instance does not exist OR is not owned by the principal,
//   return 404 — never 403. This prevents existence leakage.
//   Source: AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §3.

import (
	"context"
	"net/http"

	"github.com/compute-platform/compute-platform/internal/db"
)

// principalCtxKey is the context key for the authenticated principal ID.
type principalCtxKey struct{}

// principalFromCtx returns the principal ID injected by requirePrincipal.
// Returns ("", false) if not present.
func principalFromCtx(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(principalCtxKey{}).(string)
	return v, ok && v != ""
}

// requirePrincipal is middleware that enforces the X-Principal-ID header.
// Missing or empty header → 401 authentication_required.
// On success, the principal is stored in the request context.
// Source: AUTH_OWNERSHIP_MODEL_V1 §1, API_ERROR_CONTRACT_V1 §4.
func requirePrincipal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal := r.Header.Get("X-Principal-ID")
		if principal == "" {
			writeAPIError(w, http.StatusUnauthorized, errAuthRequired,
				"Authentication is required. Provide a valid X-Principal-ID.", "")
			return
		}
		ctx := context.WithValue(r.Context(), principalCtxKey{}, principal)
		next(w, r.WithContext(ctx))
	}
}

// loadOwnedInstance fetches an instance by ID and verifies ownership.
//
// Returns (row, true) when the instance exists and is owned by principal.
// Returns (nil, false) and writes a response for any of:
//   - instance not found → 404
//   - instance owned by a different principal → 404 (no existence leak)
//   - transient DB connectivity failure → 503 (P2-M1/WS-H1 DB-6)
//   - other DB error → 500
//
// The 404-on-mismatch rule prevents callers from probing whether an instance
// exists across account boundaries.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §3.
func (s *server) loadOwnedInstance(w http.ResponseWriter, r *http.Request, principal, id string) (*db.InstanceRow, bool) {
	row, err := s.repo.GetInstanceByID(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound,
				"The instance does not exist or you do not have access to it.", "id")
			return nil, false
		}
		s.log.Error("GetInstanceByID failed", "error", err)
		// P2-M1/WS-H1: use writeDBError so failover connectivity errors yield 503.
		writeDBError(w, err)
		return nil, false
	}

	// Ownership check: treat a different owner as not found.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	if row.OwnerPrincipalID != principal {
		writeAPIError(w, http.StatusNotFound, errInstanceNotFound,
			"The instance does not exist or you do not have access to it.", "id")
		return nil, false
	}

	return row, true
}

// loadOwnedImage fetches an image by ID and verifies ownership or grant access.
//
// Returns (row, true) when the image exists, is visible to the principal
// (owner, grantee, or public), and is accessible for the requested operation.
// Returns (nil, false) and writes a 404 response for any of:
//   - image not found → 404
//   - image not visible to principal → 404 (no existence leak)
//   - transient DB connectivity failure → 503
//   - other DB error → 500
//
// This consolidates the inline ownership checks scattered across deprecate/obsolete
// handlers into a single centralized gate. Read-only operations (GET, list)
// use GetImageForAdmissionWithGrants which includes grant resolution.
// Mutate operations (deprecate, obsolete) require direct ownership, so they
// use GetImageByID + owner check.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §3.
func (s *server) loadOwnedImage(w http.ResponseWriter, r *http.Request, principal, id string) (*db.ImageRow, bool) {
	img, err := s.repo.GetImageByID(r.Context(), id)
	if err != nil {
		s.log.Error("GetImageByID failed in loadOwnedImage", "image_id", id, "error", err)
		writeDBError(w, err)
		return nil, false
	}
	if img == nil || img.OwnerID != principal {
		writeAPIError(w, http.StatusNotFound, errImageNotFound,
			"The image does not exist or is not accessible.", "image_id")
		return nil, false
	}
	return img, true
}

// checkProjectRole verifies that principal has at least minRole in projectID.
//
// Returns true when the principal has the required role (or higher).
// Writes 404 on project-not-found or cross-account, 500/503 on DB errors.
// Returns false when access is denied — caller must return immediately.
//
// Role hierarchy: owner > admin > operator > viewer.
//
// Source: P2_PROJECT_RBAC_MODEL.md §2.4, AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) checkProjectRole(w http.ResponseWriter, r *http.Request, projectID, principal, minRole string) bool {
	proj, err := s.repo.GetProjectByID(r.Context(), projectID)
	if err != nil {
		if isProjectNotFound(err) {
			writeAPIError(w, http.StatusNotFound, errProjectNotFound,
				"Project not found.", "project_id")
			return false
		}
		s.log.Error("GetProjectByID failed in checkProjectRole", "project_id", projectID, "error", err)
		writeDBError(w, err)
		return false
	}
	if proj.CreatedBy != principal {
		writeAPIError(w, http.StatusNotFound, errProjectNotFound,
			"Project not found.", "project_id")
		return false
	}

	hasRole, err := s.repo.CheckProjectMemberHasRole(r.Context(), projectID, principal, minRole)
	if err != nil {
		s.log.Error("CheckProjectMemberHasRole failed", "project_id", projectID, "principal", principal, "error", err)
		writeDBError(w, err)
		return false
	}
	if !hasRole {
		writeAPIError(w, http.StatusForbidden, errForbidden,
			"You do not have the required role '"+minRole+"' to perform this action on this project.", "project_id")
		return false
	}
	return true
}


