package main

// instance_rbac.go — Project-aware ownership resolution for VM-P2D Slice 2.
//
// This file replaces the Phase 1 loadOwnedInstance helper with a version that
// understands Phase 2 project membership, and adds loadWritableInstance for
// lifecycle action write-gating.
//
// Phase 1 rule (preserved, all existing tests continue to pass):
//   - Direct account ownership: owner_principal_id == principal → allow.
//   - Otherwise → 404. Never 403 for cross-account misses.
//
// Phase 2 extension:
//   - If owner_principal_id belongs to a PROJECT principal and the requester
//     has an active membership (any role: OWNER, EDITOR, VIEWER) → allow read.
//   - For writes/lifecycle actions: require OWNER or EDITOR role.
//   - VIEWER attempting a write → 403 Forbidden (only 403 case per contract).
//   - Non-member or non-existent → 404.
//
// Source: P2_PROJECT_RBAC_MODEL.md §4.2 (permission matrix),
//         §4.3 (evaluation order), §7 (error rules),
//         AUTH_OWNERSHIP_MODEL_V1 §3–§4.
//
// Note: requirePrincipal, principalFromCtx, writeJSON, principalCtxKey are
// defined elsewhere in the package (server.go / main.go — not in this zip).
// errInstanceNotFound and writeDBError are defined in instance_errors.go.
// isNoRows is defined in instance_handlers.go.

import (
	"net/http"

	"github.com/compute-platform/compute-platform/internal/db"
)

// errForbidden is returned when a project VIEWER attempts a write/lifecycle
// action. This is the single case where 403 is returned instead of 404.
//
// Source: P2_PROJECT_RBAC_MODEL.md §7:
//   "Request for a project-owned resource where requester is VIEWER but action
//    requires EDITOR → 403 Forbidden."
const errForbidden = "forbidden"

// loadOwnedInstance fetches instanceID and verifies that principal can read it.
//
// Phase 2 behaviour:
//   - Direct account owner (owner_principal_id == principal): allow (Phase 1).
//   - Project member with any role (OWNER, EDITOR, VIEWER): allow.
//   - Otherwise: 404. Never 403 for visibility failures.
//
// Replaces the Phase 1 version (which only checked direct ownership).
// All existing tests that expect 404 on cross-account access continue to pass.
//
// Source: P2_PROJECT_RBAC_MODEL.md §4.3 steps 3–5, §7.
func (s *server) loadOwnedInstance(
	w http.ResponseWriter, r *http.Request,
	principal, instanceID string,
) (*db.InstanceRow, bool) {
	row, err := s.repo.GetInstanceByID(r.Context(), instanceID)
	if err != nil {
		if isNoRows(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound,
				"Instance not found.", "id")
			return nil, false
		}
		s.log.Error("GetInstanceByID failed", "instance_id", instanceID, "error", err)
		writeDBError(w, err)
		return nil, false
	}

	// Phase 1: direct account ownership.
	if row.OwnerPrincipalID == principal {
		return row, true
	}

	// Phase 2: project membership — any role permits read.
	visible, err := s.repo.IsInstanceVisibleTo(r.Context(), instanceID, principal)
	if err != nil {
		s.log.Error("IsInstanceVisibleTo failed",
			"instance_id", instanceID, "principal", principal, "error", err)
		writeDBError(w, err)
		return nil, false
	}
	if visible {
		return row, true
	}

	// No access path matched — 404, never 403.
	writeAPIError(w, http.StatusNotFound, errInstanceNotFound,
		"Instance not found.", "id")
	return nil, false
}

// loadWritableInstance fetches instanceID and verifies that principal has write
// permission (OWNER or EDITOR project role, or direct account ownership).
//
// Used by lifecycle action handlers (stop, start, reboot, delete) so that VIEWER
// members receive 403 Forbidden rather than being allowed to enqueue jobs.
//
// Error behaviour (source: P2_PROJECT_RBAC_MODEL.md §7):
//   - Instance not found or non-member → 404
//   - VIEWER role (visible but not writable) → 403
//   - OWNER or EDITOR role → allow
func (s *server) loadWritableInstance(
	w http.ResponseWriter, r *http.Request,
	principal, instanceID string,
) (*db.InstanceRow, bool) {
	row, err := s.repo.GetInstanceByID(r.Context(), instanceID)
	if err != nil {
		if isNoRows(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound,
				"Instance not found.", "id")
			return nil, false
		}
		s.log.Error("GetInstanceByID failed", "instance_id", instanceID, "error", err)
		writeDBError(w, err)
		return nil, false
	}

	// Direct account ownership → full write access (Phase 1 model).
	if row.OwnerPrincipalID == principal {
		return row, true
	}

	// Project membership: OWNER or EDITOR → write allowed.
	writable, err := s.repo.IsInstanceWritableBy(r.Context(), instanceID, principal)
	if err != nil {
		s.log.Error("IsInstanceWritableBy failed",
			"instance_id", instanceID, "principal", principal, "error", err)
		writeDBError(w, err)
		return nil, false
	}
	if writable {
		return row, true
	}

	// Distinguish VIEWER (visible, not writable) from non-member (not visible).
	// VIEWER → 403; non-member → 404.
	visible, err := s.repo.IsInstanceVisibleTo(r.Context(), instanceID, principal)
	if err != nil {
		s.log.Error("IsInstanceVisibleTo (write-gate) failed",
			"instance_id", instanceID, "principal", principal, "error", err)
		writeDBError(w, err)
		return nil, false
	}
	if visible {
		// VIEWER trying to write — only 403 case in the contract.
		writeAPIError(w, http.StatusForbidden, errForbidden,
			"Insufficient role to perform this action.", "")
		return nil, false
	}

	// Not visible — 404.
	writeAPIError(w, http.StatusNotFound, errInstanceNotFound,
		"Instance not found.", "id")
	return nil, false
}
