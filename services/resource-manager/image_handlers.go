package main

// image_handlers.go — HTTP handlers for the public image API.
//
// VM-P2C-P1: GET /v1/images, GET /v1/images/{id} (unchanged).
// VM-P2C-P2: added:
//   POST /v1/images                     → handleCreateImage        (202)
//   POST /v1/images/{id}/deprecate      → handleDeprecateImage     (200)
//   POST /v1/images/{id}/obsolete       → handleObsoleteImage      (200)
//
// VM-P2C-P3: added:
//   - family_name / family_version fields forwarded to CreateImage row from combined
//     request struct (both snapshot and import paths).
//   - imageToResponse now exposes family_name / family_version when set.
//   - Combined struct in handleCreateImage extended with family fields.
//
// VM-P3B Job 1: cross-principal private image sharing contract.
//   - POST   /v1/images/{id}/grants              → handleGrantImageAccess   (200)
//   - DELETE /v1/images/{id}/grants/{grantee_id} → handleRevokeImageAccess  (200)
//   - GET    /v1/images/{id}/grants              → handleListImageGrants     (200)
//   - GET /v1/images and GET /v1/images/{id} now use grant-aware DB methods so
//     grantees see images shared to them.
//   - Launch admission (handleCreateInstance) uses GetImageForAdmissionWithGrants
//     so grantees can launch from shared private images.
//   - Owner-only mutation semantics, 404-for-non-owner throughout.
//   - Source: image_share_handlers.go, image_share_types.go, image_share_errors.go.
//
// POST /v1/images dispatches on source_type field in the request body:
//   source_type = "SNAPSHOT" → handleCreateImageFromSnapshot
//   source_type = "IMPORT"   → handleImportImage
//   (default / missing)      → 400 invalid_request
//
// Design rules (same as snapshot_handlers.go):
//   - All routes require authentication via requirePrincipal.
//   - Handlers never call runtime directly.
//   - Ownership enforced via GetImageForAdmission (404-for-cross-account).
//   - DB errors flow through writeDBError (503/500 per DB-6 gate).
//   - Custom image creation returns 202 + job_id (async lifecycle).
//   - Lifecycle actions (deprecate, obsolete) are synchronous: they update
//     status in-place and return 200 + updated ImageResponse.
//     No worker is needed for a pure status field update.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3, §4 (image API endpoint summary),
//         AUTH_OWNERSHIP_MODEL_V1.md §3 (ownership, 404-for-cross-account),
//         API_ERROR_CONTRACT_V1.md §1 (envelope shape),
//         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md §core_contracts,
//         vm-13-01__blueprint__ §family_seam.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// Image job type constants.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4, JOB_MODEL_V1 §3.
const (
	jobTypeImageCreate = "IMAGE_CREATE"
	jobTypeImageImport = "IMAGE_IMPORT"
)

// imageJobMaxAttempts per image job type.
// Image validation may involve I/O; allow more retries than lifecycle ops.
var imageJobMaxAttempts = map[string]int{
	jobTypeImageCreate: 3,
	jobTypeImageImport: 5,
}

// ── Route registration ────────────────────────────────────────────────────────

// registerImageRoutes registers the public image API routes.
func (s *server) registerImageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/images", requirePrincipal(s.handleImageRoot))
	mux.HandleFunc("/v1/images/", requirePrincipal(s.handleImageByID))
}

// handleImageRoot dispatches GET /v1/images.
// POST /v1/images is not registered on this mux entry; callers expecting
// image creation should use the dedicated POST route when available.
func (s *server) handleImageRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListImages(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleImageByID dispatches based on method and sub-path.
// Handles:
//
//	GET    /v1/images/{id}
//	POST   /v1/images/{id}/deprecate
//	POST   /v1/images/{id}/obsolete
//
// VM-P3B Job 1 — share grant routes:
//
//	POST   /v1/images/{id}/grants
//	GET    /v1/images/{id}/grants
//	DELETE /v1/images/{id}/grants/{grantee_principal_id}
func (s *server) handleImageByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/images/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	// Split on first "/" to detect sub-paths.
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		// /v1/images/{id}
		switch r.Method {
		case http.MethodGet:
			s.handleGetImage(w, r, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// /v1/images/{id}/{action_and_rest}
	actionRest := parts[1]

	switch {
	case actionRest == "deprecate" && r.Method == http.MethodPost:
		s.handleDeprecateImage(w, r, id)

	case actionRest == "obsolete" && r.Method == http.MethodPost:
		s.handleObsoleteImage(w, r, id)

	// VM-P3B: GET /v1/images/{id}/grants
	case actionRest == "grants" && r.Method == http.MethodGet:
		s.handleListImageGrants(w, r, id)

	// VM-P3B: POST /v1/images/{id}/grants
	case actionRest == "grants" && r.Method == http.MethodPost:
		s.handleGrantImageAccess(w, r, id)

	// VM-P3B: DELETE /v1/images/{id}/grants/{grantee_principal_id}
	case strings.HasPrefix(actionRest, "grants/") && r.Method == http.MethodDelete:
		granteePrincipalID := strings.TrimPrefix(actionRest, "grants/")
		if granteePrincipalID == "" {
			http.NotFound(w, r)
			return
		}
		s.handleRevokeImageAccess(w, r, id, granteePrincipalID)

	default:
		http.NotFound(w, r)
	}
}

// ── GET /v1/images ────────────────────────────────────────────────────────────

// handleListImages handles GET /v1/images.
// VM-P3B Job 1: uses ListImagesByPrincipalWithGrants so grantees see images
// shared to them in addition to public images and images they own.
// Source: VM-P3B Job 1 §4.
func (s *server) handleListImages(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	rows, err := s.repo.ListImagesByPrincipalWithGrants(r.Context(), principal)
	if err != nil {
		s.log.Error("ListImagesByPrincipalWithGrants failed", "error", err)
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
// VM-P3B Job 1: uses GetImageForAdmissionWithGrants so grantees can fetch
// images shared to them. Non-grantees still get 404.
// Source: VM-P3B Job 1 §5.
func (s *server) handleGetImage(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	img, err := s.repo.GetImageForAdmissionWithGrants(r.Context(), id, principal)
	if err != nil {
		s.log.Error("GetImageForAdmissionWithGrants failed", "image_id", id, "error", err)
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

// ── POST /v1/images ───────────────────────────────────────────────────────────

// createImageRouteRequest is used only to read the source_type field for dispatch.
type createImageRouteRequest struct {
	SourceType string `json:"source_type"`
}

// handleCreateImage handles POST /v1/images.
// Dispatches on source_type field:
//   - "SNAPSHOT" → handleCreateImageFromSnapshot
//   - "IMPORT"   → handleImportImage
//
// VM-P2C-P3: combined struct extended with family_name and family_version.
// These fields are forwarded to the appropriate sub-handler request type.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (custom image flows),
//         vm-13-01__blueprint__ §family_seam.
func (s *server) handleCreateImage(w http.ResponseWriter, r *http.Request) {
	// Decode a combined struct containing all fields of both request types plus
	// the new family fields. The validator checks only the relevant subset.
	var combined struct {
		SourceType       string  `json:"source_type"`
		Name             string  `json:"name"`
		SourceSnapshotID string  `json:"source_snapshot_id"`
		ImportURL        string  `json:"import_url"`
		OSFamily         string  `json:"os_family"`
		OSVersion        string  `json:"os_version"`
		Architecture     string  `json:"architecture"`
		MinDiskGB        *int    `json:"min_disk_gb"`
		// VM-P2C-P3: family membership fields.
		FamilyName    *string `json:"family_name"`
		FamilyVersion *int    `json:"family_version"`
	}

	if err := json.NewDecoder(r.Body).Decode(&combined); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"Request body is not valid JSON.", "")
		return
	}

	switch combined.SourceType {
	case db.ImageSourceTypeSnapshot:
		req := &CreateImageFromSnapshotRequest{
			Name:             combined.Name,
			SourceSnapshotID: combined.SourceSnapshotID,
			OSFamily:         combined.OSFamily,
			OSVersion:        combined.OSVersion,
			Architecture:     combined.Architecture,
			MinDiskGB:        combined.MinDiskGB,
			FamilyName:       combined.FamilyName,
			FamilyVersion:    combined.FamilyVersion,
		}
		s.handleCreateImageFromSnapshot(w, r, req)

	case db.ImageSourceTypeImport:
		minDiskGB := 0
		if combined.MinDiskGB != nil {
			minDiskGB = *combined.MinDiskGB
		}
		req := &ImportImageRequest{
			Name:          combined.Name,
			ImportURL:     combined.ImportURL,
			OSFamily:      combined.OSFamily,
			OSVersion:     combined.OSVersion,
			Architecture:  combined.Architecture,
			MinDiskGB:     minDiskGB,
			FamilyName:    combined.FamilyName,
			FamilyVersion: combined.FamilyVersion,
		}
		s.handleImportImage(w, r, req)

	default:
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"source_type must be one of: SNAPSHOT, IMPORT.", "source_type")
	}
}

// handleCreateImageFromSnapshot handles POST /v1/images with source_type=SNAPSHOT.
//
// Flow:
//  1. Validate request fields (including optional family_name/version).
//  2. Load and verify the source snapshot (owner + status=available).
//  3. Insert image record in PENDING_VALIDATION status with family fields when set.
//  4. Enqueue IMAGE_CREATE job.
//  5. Return 202 + image resource + job_id.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.6, vm-13-01__blueprint__ §family_seam.
func (s *server) handleCreateImageFromSnapshot(w http.ResponseWriter, r *http.Request, req *CreateImageFromSnapshotRequest) {
	principal, _ := principalFromCtx(r.Context())

	// 1. Field validation.
	if errs := validateCreateImageFromSnapshotRequest(req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	// 2. Load source snapshot — must be owned by caller and in available status.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.6 (snapshot must be available).
	// AUTH: use ownership check (404-for-cross-account).
	snap, err := s.repo.GetSnapshotByID(r.Context(), req.SourceSnapshotID)
	if err != nil {
		s.log.Error("GetSnapshotByID failed", "snapshot_id", req.SourceSnapshotID, "error", err)
		writeDBError(w, err)
		return
	}
	if snap == nil || snap.OwnerPrincipalID != principal {
		// Not found or not owned — same 422 response per AUTH contract (not existence leak).
		writeAPIError(w, http.StatusUnprocessableEntity, errImageSnapshotNotFound,
			"Snapshot '"+req.SourceSnapshotID+"' does not exist or is not accessible.", "source_snapshot_id")
		return
	}
	if snap.Status != db.SnapshotStatusAvailable {
		writeAPIError(w, http.StatusUnprocessableEntity, errImageSnapshotNotAvailable,
			"Snapshot '"+req.SourceSnapshotID+"' must be in 'available' status to create an image from it (current: "+snap.Status+").", "source_snapshot_id")
		return
	}

	// Resolve min_disk_gb: default to snapshot size_gb when omitted.
	minDiskGB := snap.SizeGB
	if req.MinDiskGB != nil && *req.MinDiskGB > 0 {
		minDiskGB = *req.MinDiskGB
	}

	// 3. Insert image record.
	// family_name and family_version are forwarded from request when set.
	imageID := idgen.New("img")
	snapID := req.SourceSnapshotID
	imageRow := &db.ImageRow{
		ID:               imageID,
		Name:             req.Name,
		OSFamily:         req.OSFamily,
		OSVersion:        req.OSVersion,
		Architecture:     req.Architecture,
		OwnerID:          principal,
		Visibility:       db.ImageVisibilityPrivate,
		SourceType:       db.ImageSourceTypeSnapshot,
		StorageURL:       "", // set by worker on completion
		MinDiskGB:        minDiskGB,
		Status:           db.ImageStatusPendingValidation,
		ValidationStatus: "pending",
		SourceSnapshotID: &snapID,
		FamilyName:       req.FamilyName,
		FamilyVersion:    req.FamilyVersion,
	}
	if err := s.repo.CreateImage(r.Context(), imageRow); err != nil {
		s.log.Error("CreateImage failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}

	// 4. Enqueue IMAGE_CREATE job.
	jobID := idgen.New(idgen.PrefixJob)
	if err := s.repo.InsertImageJob(r.Context(), &db.JobRow{
		ID:          jobID,
		ImageID:     &imageID,
		JobType:     jobTypeImageCreate,
		MaxAttempts: imageJobMaxAttempts[jobTypeImageCreate],
	}); err != nil {
		s.log.Error("InsertImageJob failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}

	// 5. Return 202 using the inserted row directly.
	writeJSON(w, http.StatusAccepted, CreateImageFromSnapshotResponse{
		Image: imageToResponse(imageRow),
		JobID: jobID,
	})
}

// handleImportImage handles POST /v1/images with source_type=IMPORT.
//
// Flow:
//  1. Validate request fields (including optional family_name/version).
//  2. Insert image record in PENDING_VALIDATION status with family fields when set.
//  3. Enqueue IMAGE_IMPORT job.
//  4. Return 202 + image resource + job_id.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (import lifecycle),
//         vm-13-01__blueprint__ §family_seam.
func (s *server) handleImportImage(w http.ResponseWriter, r *http.Request, req *ImportImageRequest) {
	principal, _ := principalFromCtx(r.Context())

	// 1. Field validation.
	if errs := validateImportImageRequest(req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	// 2. Insert image record.
	imageID := idgen.New("img")
	importURL := req.ImportURL
	imageRow := &db.ImageRow{
		ID:               imageID,
		Name:             req.Name,
		OSFamily:         req.OSFamily,
		OSVersion:        req.OSVersion,
		Architecture:     req.Architecture,
		OwnerID:          principal,
		Visibility:       db.ImageVisibilityPrivate,
		SourceType:       db.ImageSourceTypeImport,
		StorageURL:       "", // set by worker on completion
		MinDiskGB:        req.MinDiskGB,
		Status:           db.ImageStatusPendingValidation,
		ValidationStatus: "pending",
		ImportURL:        &importURL,
		FamilyName:       req.FamilyName,
		FamilyVersion:    req.FamilyVersion,
	}
	if err := s.repo.CreateImage(r.Context(), imageRow); err != nil {
		s.log.Error("CreateImage failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}

	// 3. Enqueue IMAGE_IMPORT job.
	jobID := idgen.New(idgen.PrefixJob)
	if err := s.repo.InsertImageJob(r.Context(), &db.JobRow{
		ID:          jobID,
		ImageID:     &imageID,
		JobType:     jobTypeImageImport,
		MaxAttempts: imageJobMaxAttempts[jobTypeImageImport],
	}); err != nil {
		s.log.Error("InsertImageJob failed", "image_id", imageID, "error", err)
		writeDBError(w, err)
		return
	}

	// 4. Return 202 using the inserted row directly.
	writeJSON(w, http.StatusAccepted, ImportImageResponse{
		Image: imageToResponse(imageRow),
		JobID: jobID,
	})
}

// ── POST /v1/images/{id}/deprecate ───────────────────────────────────────────

// handleDeprecateImage handles POST /v1/images/{id}/deprecate.
//
// Transitions ACTIVE → DEPRECATED (synchronous status update).
// Only the owning principal may deprecate a private image.
// PUBLIC images (platform images): only if owned by caller (platform admin path;
// standard users will hit 404 since they don't own platform images).
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
func (s *server) handleDeprecateImage(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	img, err := s.repo.GetImageByID(r.Context(), id)
	if err != nil {
		s.log.Error("GetImageByID (deprecate) failed", "image_id", id, "error", err)
		writeDBError(w, err)
		return
	}
	// Ownership check: 404-for-non-owner (not 403).
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	if img == nil || img.OwnerID != principal {
		writeAPIError(w, http.StatusNotFound, errImageNotFound,
			"The image does not exist or is not accessible.", "image_id")
		return
	}
	// Only ACTIVE images may be deprecated.
	if img.Status != db.ImageStatusActive {
		writeAPIError(w, http.StatusUnprocessableEntity, errImageInvalidState,
			"Image '"+id+"' cannot be deprecated (current status: "+img.Status+"). Only ACTIVE images may be deprecated.", "image_id")
		return
	}

	now := time.Now()
	if err := s.repo.UpdateImageStatus(r.Context(), id, db.ImageStatusDeprecated, &now, nil); err != nil {
		s.log.Error("UpdateImageStatus (deprecate) failed", "image_id", id, "error", err)
		writeDBError(w, err)
		return
	}

	updated, err := s.repo.GetImageByID(r.Context(), id)
	if err != nil {
		s.log.Error("GetImageByID after deprecate failed", "image_id", id, "error", err)
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeprecateImageResponse{Image: imageToResponse(updated)})
}

// ── POST /v1/images/{id}/obsolete ────────────────────────────────────────────

// handleObsoleteImage handles POST /v1/images/{id}/obsolete.
//
// Transitions ACTIVE or DEPRECATED → OBSOLETE (synchronous status update).
// OBSOLETE images are immediately blocked from launch.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
func (s *server) handleObsoleteImage(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	img, err := s.repo.GetImageByID(r.Context(), id)
	if err != nil {
		s.log.Error("GetImageByID (obsolete) failed", "image_id", id, "error", err)
		writeDBError(w, err)
		return
	}
	// Ownership check: 404-for-non-owner.
	if img == nil || img.OwnerID != principal {
		writeAPIError(w, http.StatusNotFound, errImageNotFound,
			"The image does not exist or is not accessible.", "image_id")
		return
	}
	// ACTIVE and DEPRECATED images may be obsoleted.
	if img.Status != db.ImageStatusActive && img.Status != db.ImageStatusDeprecated {
		writeAPIError(w, http.StatusUnprocessableEntity, errImageInvalidState,
			"Image '"+id+"' cannot be obsoleted (current status: "+img.Status+"). Only ACTIVE or DEPRECATED images may be obsoleted.", "image_id")
		return
	}

	now := time.Now()
	if err := s.repo.UpdateImageStatus(r.Context(), id, db.ImageStatusObsolete, nil, &now); err != nil {
		s.log.Error("UpdateImageStatus (obsolete) failed", "image_id", id, "error", err)
		writeDBError(w, err)
		return
	}

	updated, err := s.repo.GetImageByID(r.Context(), id)
	if err != nil {
		s.log.Error("GetImageByID after obsolete failed", "image_id", id, "error", err)
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ObsoleteImageResponse{Image: imageToResponse(updated)})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// imageToResponse maps a db.ImageRow to the canonical ImageResponse.
//
// Internal fields (storage_url, owner_id, validation_status, import_url) are
// not exposed in the public API response.
//
// VM-P2C-P2: source_snapshot_id is exposed for SNAPSHOT-sourced images.
// VM-P2C-P3: family_name and family_version are exposed when set (non-nil).
//
// Source: image_types.go, P2_IMAGE_SNAPSHOT_MODEL.md §3.3,
//         vm-13-01__blueprint__ §family_seam.
func imageToResponse(row *db.ImageRow) ImageResponse {
	return ImageResponse{
		ID:               row.ID,
		Name:             row.Name,
		OSFamily:         row.OSFamily,
		OSVersion:        row.OSVersion,
		Architecture:     row.Architecture,
		Visibility:       row.Visibility,
		SourceType:       row.SourceType,
		MinDiskGB:        row.MinDiskGB,
		Status:           row.Status,
		SourceSnapshotID: row.SourceSnapshotID,
		FamilyName:       row.FamilyName,
		FamilyVersion:    row.FamilyVersion,
		DeprecatedAt:     row.DeprecatedAt,
		ObsoletedAt:      row.ObsoletedAt,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}
