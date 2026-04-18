package main

// image_share_handlers.go — HTTP handlers for image share grant management.
//
// VM-P3B Job 1: cross-principal private image sharing contract.
//
// Routes (wired in registerImageRoutes via handleImageByID sub-path dispatch):
//
//   POST   /v1/images/{id}/grants              → handleGrantImageAccess   (200)
//   DELETE /v1/images/{id}/grants/{grantee_id} → handleRevokeImageAccess  (200)
//   GET    /v1/images/{id}/grants              → handleListImageGrants     (200)
//
// Authorization model:
//   - All three endpoints require the caller to be the image owner.
//   - Non-owners receive 404 (not 403). Existence is not leaked.
//   - Source: AUTH_OWNERSHIP_MODEL_V1.md §3.
//
// Grantee semantics:
//   - grantee_principal_id is a raw principal UUID.
//   - Re-granting the same grantee is idempotent (200, existing grant returned).
//   - Revoking a non-existent grant is idempotent (200, revoked=false).
//   - Grants apply to PRIVATE images only; PUBLIC images are rejected with 422.
//   - Granting the image owner themselves is rejected with 422.
//
// Visibility extension (list/get/launch):
//   - handleListImages and handleGetImage now use ListImagesByPrincipalWithGrants
//     and GetImageForAdmissionWithGrants so grantees see shared images.
//   - handleCreateInstance image admission calls GetImageForAdmissionWithGrants
//     so grantees can launch from shared private images.
//   - Source: VM-P3B Job 1 §3–§6.
//
// Ownership semantics preserved:
//   - Shared images remain owned by the sharer (owner_id unchanged).
//   - Instances launched by grantees are owned by the grantee (owner_principal_id
//     set by handleCreateInstance from the caller's principal — unchanged).
//   - Source: VM-P3B Job 1 §7.
//
// Revoke semantics:
//   - DELETE removes the grant; future GET/LIST/launch calls by the grantee
//     will return 404/admission failure. Running instances are unaffected.
//   - Source: VM-P3B Job 1 §8.
//
// Grantee cleanup:
//   - ON DELETE CASCADE on image_share_grants.grantee_principal_id handles
//     automatic grant removal when a principal is deleted. No handler code needed.
//   - Source: db/migrations/0013_image_share_grants.up.sql, VM-P3B Job 1 §9.
//
// Source: API_ERROR_CONTRACT_V1.md §1 (envelope shape),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.7 (visibility),
//         AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-cross-account).

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── Route wiring ──────────────────────────────────────────────────────────────
//
// handleImageByID already splits the sub-path after the image ID.
// The share-grant routes extend the existing action dispatch in that function.
// See the patched handleImageByID below.

// handleGrantImageAccess handles POST /v1/images/{id}/grants.
//
// Flow:
//  1. Load image — must exist and be owned by caller (404-for-non-owner).
//  2. Reject if image is PUBLIC (no grant needed).
//  3. Reject self-grant (owner granting themselves).
//  4. Validate grantee_principal_id is present.
//  5. Insert grant (idempotent ON CONFLICT DO NOTHING).
//  6. Return 200 + grant resource.
func (s *server) handleGrantImageAccess(w http.ResponseWriter, r *http.Request, imageID string) {
	principal, _ := principalFromCtx(r.Context())

	// 1. Load image and enforce ownership (404-for-non-owner).
	img, err := s.repo.GetImageByID(r.Context(), imageID)
	if err != nil {
		s.log.Error("GetImageByID (grant) failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}
	if img == nil || img.OwnerID != principal {
		writeAPIError(w, http.StatusNotFound, errImageGrantNotOwner,
			"The image does not exist or is not accessible.", "image_id")
		return
	}

	// 2. Reject grants on PUBLIC images.
	if img.Visibility == db.ImageVisibilityPublic {
		writeAPIError(w, http.StatusUnprocessableEntity, errImageGrantPublicImage,
			"Image '"+imageID+"' is PUBLIC and does not require share grants.", "image_id")
		return
	}

	// 3–4. Decode and validate request.
	var req GrantImageAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"Request body is not valid JSON.", "")
		return
	}
	if strings.TrimSpace(req.GranteePrincipalID) == "" {
		writeAPIError(w, http.StatusBadRequest, errImageGrantGranteeRequired,
			"grantee_principal_id is required.", "grantee_principal_id")
		return
	}
	if req.GranteePrincipalID == principal {
		writeAPIError(w, http.StatusUnprocessableEntity, errImageGrantSelfGrant,
			"The image owner cannot grant access to themselves.", "grantee_principal_id")
		return
	}

	// 5. Insert grant. Idempotent: re-grant returns nil error.
	grantID := idgen.New("igrant")
	grantRow := &db.ImageGrantRow{
		ID:                 grantID,
		ImageID:            imageID,
		OwnerPrincipalID:   principal,
		GranteePrincipalID: req.GranteePrincipalID,
	}
	if err := s.repo.CreateImageGrant(r.Context(), grantRow); err != nil {
		s.log.Error("CreateImageGrant failed", "image_id", imageID, "grantee", req.GranteePrincipalID, "error", err)
		writeDBError(w, err)
		return
	}

	// Re-fetch to get actual created_at (may differ if ON CONFLICT hit).
	actual, err := s.repo.GetImageGrant(r.Context(), imageID, req.GranteePrincipalID)
	if err != nil {
		s.log.Error("GetImageGrant after create failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, GrantImageAccessResponse{
		Grant: imageGrantToResponse(actual),
	})
}

// handleRevokeImageAccess handles DELETE /v1/images/{id}/grants/{grantee_principal_id}.
//
// Flow:
//  1. Load image — must exist and be owned by caller (404-for-non-owner).
//  2. Delete grant row (idempotent: no-grant returns revoked=false).
//  3. Return 200 + { "revoked": true|false }.
func (s *server) handleRevokeImageAccess(w http.ResponseWriter, r *http.Request, imageID, granteePrincipalID string) {
	principal, _ := principalFromCtx(r.Context())

	// 1. Load image and enforce ownership.
	img, err := s.repo.GetImageByID(r.Context(), imageID)
	if err != nil {
		s.log.Error("GetImageByID (revoke) failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}
	if img == nil || img.OwnerID != principal {
		writeAPIError(w, http.StatusNotFound, errImageGrantNotOwner,
			"The image does not exist or is not accessible.", "image_id")
		return
	}

	// 2. Delete grant (idempotent).
	revoked, err := s.repo.RevokeImageGrant(r.Context(), imageID, granteePrincipalID)
	if err != nil {
		s.log.Error("RevokeImageGrant failed", "image_id", imageID, "grantee", granteePrincipalID, "error", err)
		writeDBError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, RevokeImageAccessResponse{Revoked: revoked})
}

// handleListImageGrants handles GET /v1/images/{id}/grants.
//
// Returns all active grants for the image. Owner-only endpoint.
//
// Flow:
//  1. Load image — must exist and be owned by caller (404-for-non-owner).
//  2. List all grants for image ordered by created_at ASC.
//  3. Return 200 + grants array.
func (s *server) handleListImageGrants(w http.ResponseWriter, r *http.Request, imageID string) {
	principal, _ := principalFromCtx(r.Context())

	// 1. Load image and enforce ownership.
	img, err := s.repo.GetImageByID(r.Context(), imageID)
	if err != nil {
		s.log.Error("GetImageByID (list grants) failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}
	if img == nil || img.OwnerID != principal {
		writeAPIError(w, http.StatusNotFound, errImageGrantNotOwner,
			"The image does not exist or is not accessible.", "image_id")
		return
	}

	// 2. List grants.
	grants, err := s.repo.ListImageGrants(r.Context(), imageID)
	if err != nil {
		s.log.Error("ListImageGrants failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}

	out := make([]ImageGrantResponse, 0, len(grants))
	for _, g := range grants {
		out = append(out, imageGrantToResponse(g))
	}
	writeJSON(w, http.StatusOK, ListImageGrantsResponse{
		Grants: out,
		Total:  len(out),
	})
}

// ── Helper ────────────────────────────────────────────────────────────────────

// imageGrantToResponse maps a db.ImageGrantRow to the API response shape.
// owner_principal_id is intentionally excluded from the response.
func imageGrantToResponse(g *db.ImageGrantRow) ImageGrantResponse {
	return ImageGrantResponse{
		ID:                 g.ID,
		ImageID:            g.ImageID,
		GranteePrincipalID: g.GranteePrincipalID,
		CreatedAt:          g.CreatedAt,
	}
}
