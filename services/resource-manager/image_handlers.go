package main

// image_handlers.go — HTTP handlers for the public image API.
//
// VM-P2C-P1: read-only image list and get endpoints.
//
// Routes registered:
//   GET /v1/images         → handleListImages   (200)
//   GET /v1/images/{id}    → handleGetImage     (200 | 404)
//
// Design rules (same as snapshot_handlers.go and volume_handlers.go):
//   - All routes require authentication via requirePrincipal.
//   - Handlers never call runtime directly.
//   - Ownership/visibility enforced via GetImageForAdmission (404-for-cross-account).
//   - DB errors flow through writeDBError (503/500 per DB-6 gate).
//   - No mutating operations in this slice (no POST/PATCH/DELETE).
//
// Explicit non-goals for this slice (VM-P2C later slices):
//   - POST /v1/images          (custom image from snapshot)
//   - POST /v1/images/{id}/deregister
//   - DELETE /v1/images/{id}
//   - Image family / alias resolution
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (image API endpoint summary),
//         AUTH_OWNERSHIP_MODEL_V1.md §3 (ownership, 404-for-cross-account),
//         API_ERROR_CONTRACT_V1.md §1 (envelope shape),
//         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md §core_contracts.

import (
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Route registration ────────────────────────────────────────────────────────

// registerImageRoutes registers the public image API routes.
func (s *server) registerImageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/images", requirePrincipal(s.handleImageRoot))
	mux.HandleFunc("/v1/images/", requirePrincipal(s.handleImageByID))
}

// handleImageRoot dispatches GET /v1/images.
func (s *server) handleImageRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListImages(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleImageByID dispatches GET /v1/images/{id}.
// No sub-paths are handled in this slice.
func (s *server) handleImageByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/images/")
	// Strip any trailing subpath — not handled in this slice.
	if idx := strings.Index(id, "/"); idx != -1 {
		http.NotFound(w, r)
		return
	}
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleGetImage(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── GET /v1/images ────────────────────────────────────────────────────────────

// handleListImages handles GET /v1/images.
// Returns 200 + ListImagesResponse containing:
//   - All PUBLIC images (platform-provided and any public custom images).
//   - PRIVATE images owned by the calling principal.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4 (GET /v1/images),
//
//	AUTH_OWNERSHIP_MODEL_V1.md §3.
func (s *server) handleListImages(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	rows, err := s.repo.ListImagesByPrincipal(r.Context(), principal)
	if err != nil {
		s.log.Error("ListImagesByPrincipal failed", "error", err)
		writeDBError(w, err)
		return
	}

	out := make([]ImageResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, imageToResponse(row))
	}
	writeJSON(w, http.StatusOK, ListImagesResponse{
		Images: out,
		Total:  len(out),
	})
}

// ── GET /v1/images/{id} ───────────────────────────────────────────────────────

// handleGetImage handles GET /v1/images/{id}.
// Returns 200 + ImageResponse, or 404 if the image does not exist or is not
// accessible to the calling principal.
//
// Visibility enforcement: PRIVATE images are 404 for non-owners.
// Source: AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-cross-account),
//
//	P2_IMAGE_SNAPSHOT_MODEL.md §3.7.
func (s *server) handleGetImage(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	img, err := s.repo.GetImageForAdmission(r.Context(), id, principal)
	if err != nil {
		s.log.Error("GetImageForAdmission failed", "image_id", id, "error", err)
		writeDBError(w, err)
		return
	}
	if img == nil {
		writeAPIError(w, http.StatusNotFound, errImageNotFound,
			"The image does not exist or is not accessible.", "image_id")
		return
	}

	writeJSON(w, http.StatusOK, imageToResponse(img))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// imageToResponse maps a db.ImageRow to the canonical ImageResponse.
// Internal fields (storage_url, owner_id, validation_status, source_snapshot_id)
// are not exposed in the public API response.
// Source: image_types.go, P2_IMAGE_SNAPSHOT_MODEL.md §3.3.
func imageToResponse(row *db.ImageRow) ImageResponse {
	return ImageResponse{
		ID:           row.ID,
		Name:         row.Name,
		OSFamily:     row.OSFamily,
		OSVersion:    row.OSVersion,
		Architecture: row.Architecture,
		Visibility:   row.Visibility,
		SourceType:   row.SourceType,
		MinDiskGB:    row.MinDiskGB,
		Status:       row.Status,
		DeprecatedAt: row.DeprecatedAt,
		ObsoletedAt:  row.ObsoletedAt,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}
