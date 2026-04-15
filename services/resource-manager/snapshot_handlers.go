package main

// snapshot_handlers.go — HTTP handlers for the public snapshot API.
//
// VM-P2B-S2: snapshot create/list/get/delete + restore-to-volume.
// VM-P2B-S3: handleRestoreSnapshot now returns RestoreSnapshotResponse{Volume, JobID}
//            instead of {volume_id, job_id}.
//
// Routes registered:
//   POST   /v1/snapshots                    → handleCreateSnapshot   (202 + Job)
//   GET    /v1/snapshots                    → handleListSnapshots    (200)
//   GET    /v1/snapshots/{id}               → handleGetSnapshot      (200)
//   DELETE /v1/snapshots/{id}               → handleDeleteSnapshot   (202 + Job)
//   POST   /v1/snapshots/{id}/restore       → handleRestoreSnapshot  (202 + Job)
//
// Design rules (same as volume_handlers.go):
//   - Handlers never call runtime directly.
//   - All mutating operations enqueue a job and return 202.
//   - Ownership enforced via loadOwnedSnapshot (404-for-cross-account).
//   - DB errors flow through writeDBError.
//   - Validation uses fieldErr / writeAPIErrors (no new framework).
//
// Explicit non-goals (VM-P2C, not here):
//   - POST /v1/images          (custom image from snapshot)
//   - image lifecycle endpoints
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (snapshot lifecycle), §4 (API endpoints),
//         JOB_MODEL_V1, AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §1.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// Snapshot job type constants.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4.
const (
	jobTypeSnapshotCreate = "SNAPSHOT_CREATE"
	jobTypeSnapshotDelete = "SNAPSHOT_DELETE"
	jobTypeVolumeRestore  = "VOLUME_RESTORE"
)

// snapshotJobMaxAttempts per snapshot job type.
// Snapshot I/O may be slow; allow more retries than volume ops.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.4 (creating state may take time).
var snapshotJobMaxAttempts = map[string]int{
	jobTypeSnapshotCreate: 3,
	jobTypeSnapshotDelete: 5,
	jobTypeVolumeRestore:  3,
}

// ── Route registration ────────────────────────────────────────────────────────

// registerSnapshotRoutes registers the public snapshot API routes.
func (s *server) registerSnapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/snapshots", requirePrincipal(s.handleSnapshotRoot))
	mux.HandleFunc("/v1/snapshots/", requirePrincipal(s.handleSnapshotByID))
}

// handleSnapshotRoot dispatches POST /v1/snapshots and GET /v1/snapshots.
func (s *server) handleSnapshotRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateSnapshot(w, r)
	case http.MethodGet:
		s.handleListSnapshots(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSnapshotByID dispatches based on method and optional sub-path.
// Handles:
//
//	GET    /v1/snapshots/{id}
//	DELETE /v1/snapshots/{id}
//	POST   /v1/snapshots/{id}/restore
func (s *server) handleSnapshotByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/snapshots/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	switch {
	case r.Method == http.MethodGet && sub == "":
		s.handleGetSnapshot(w, r, id)
	case r.Method == http.MethodDelete && sub == "":
		s.handleDeleteSnapshot(w, r, id)
	case r.Method == http.MethodPost && sub == "restore":
		s.handleRestoreSnapshot(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── POST /v1/snapshots ────────────────────────────────────────────────────────

// handleCreateSnapshot handles POST /v1/snapshots.
// Validates source, inserts snapshot row in 'pending' status, enqueues
// SNAPSHOT_CREATE job. Returns 202 + CreateSnapshotResponse.
//
// Admission checks:
//   - source_volume_id: volume must exist, be owned by principal, be in
//     'available' or 'in_use' state.
//   - source_instance_id: instance must exist, be owned by principal, be in
//     'running' or 'stopped' state (SNAP-I-4).
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6, §2.9 SNAP-I-4.
func (s *server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	var req CreateSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"Request body is not valid JSON.", "")
		return
	}
	if errs := validateCreateSnapshotRequest(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	// Determine size_gb and validate source state.
	var sizeGB int
	var sourceVolID *string
	var sourceInstID *string

	if req.SourceVolumeID != nil {
		// Volume snapshot path.
		vol, ok := s.loadOwnedVolume(w, r, principal, *req.SourceVolumeID)
		if !ok {
			return
		}
		// Source volume must be available or in_use.
		// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6.
		if vol.Status != "available" && vol.Status != "in_use" {
			writeAPIError(w, http.StatusConflict, errSnapshotSourceInvalidState,
				"Cannot snapshot a volume in state '"+vol.Status+"'. Volume must be 'available' or 'in_use'.", "source_volume_id")
			return
		}
		sizeGB = vol.SizeGB
		sourceVolID = &vol.ID
	} else {
		// Instance root-disk snapshot path.
		inst, ok := s.loadOwnedInstance(w, r, principal, *req.SourceInstanceID)
		if !ok {
			return
		}
		// Instance must be running or stopped — transitional states rejected.
		// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6, SNAP-I-4.
		if inst.VMState != "running" && inst.VMState != "stopped" {
			writeAPIError(w, http.StatusConflict, errSnapshotSourceInvalidState,
				"Cannot snapshot an instance in state '"+inst.VMState+"'. Instance must be 'running' or 'stopped'.", "source_instance_id")
			return
		}
		// Derive size_gb from the instance's root disk.
		disk, err := s.repo.GetRootDiskByInstanceID(r.Context(), inst.ID)
		if err != nil || disk == nil {
			s.log.Error("GetRootDiskByInstanceID failed for snapshot", "instance_id", inst.ID, "error", err)
			writeInternalError(w)
			return
		}
		sizeGB = disk.SizeGB
		instID := inst.ID
		sourceInstID = &instID
	}

	snapID := idgen.New("snap")
	snapRow := &db.SnapshotRow{
		ID:               snapID,
		OwnerPrincipalID: principal,
		DisplayName:      req.Name,
		Region:           s.region,
		SourceVolumeID:   sourceVolID,
		SourceInstanceID: sourceInstID,
		SizeGB:           sizeGB,
		Encrypted:        false,
	}
	if err := s.repo.CreateSnapshot(r.Context(), snapRow); err != nil {
		s.log.Error("CreateSnapshot failed", "error", err)
		writeDBError(w, err)
		return
	}

	jobID := idgen.New(idgen.PrefixJob)
	jobRow := &db.JobRow{
		ID:          jobID,
		SnapshotID:  &snapID,
		JobType:     jobTypeSnapshotCreate,
		MaxAttempts: snapshotJobMaxAttempts[jobTypeSnapshotCreate],
	}
	if err := s.repo.InsertSnapshotJob(r.Context(), jobRow); err != nil {
		s.log.Error("InsertSnapshotJob (create) failed", "error", err)
		writeDBError(w, err)
		return
	}

	created, err := s.repo.GetSnapshotByID(r.Context(), snapID)
	if err != nil || created == nil {
		s.log.Error("GetSnapshotByID after insert failed", "error", err)
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusAccepted, CreateSnapshotResponse{
		Snapshot: snapshotToResponse(created),
		JobID:    jobID,
	})
}

// ── GET /v1/snapshots ─────────────────────────────────────────────────────────

// handleListSnapshots handles GET /v1/snapshots.
// Returns 200 + ListSnapshotsResponse scoped to the calling principal.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4, AUTH_OWNERSHIP_MODEL_V1 §4.
func (s *server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	rows, err := s.repo.ListSnapshotsByOwner(r.Context(), principal)
	if err != nil {
		s.log.Error("ListSnapshotsByOwner failed", "error", err)
		writeDBError(w, err)
		return
	}

	out := make([]SnapshotResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, snapshotToResponse(row))
	}

	writeJSON(w, http.StatusOK, ListSnapshotsResponse{
		Snapshots: out,
		Total:     len(out),
	})
}

// ── GET /v1/snapshots/{id} ────────────────────────────────────────────────────

// handleGetSnapshot handles GET /v1/snapshots/{id}.
// Returns 200 + SnapshotResponse or 404.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §4, AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) handleGetSnapshot(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	snap, ok := s.loadOwnedSnapshot(w, r, principal, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, snapshotToResponse(snap))
}

// ── DELETE /v1/snapshots/{id} ─────────────────────────────────────────────────

// handleDeleteSnapshot handles DELETE /v1/snapshots/{id}.
// Returns 202 + SnapshotLifecycleResponse or 409 when snapshot is in a
// transitional state or has active dependents.
//
// Checks:
//   - snapshot must be in 'available' or 'error' state.
//   - no active SNAPSHOT_DELETE job already in flight (double-enqueue guard).
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5, §2.7, §2.9 SNAP-I-2 and SNAP-I-3.
//
// Note: SNAP-I-3 (cannot delete while volumes created from it still exist) is
// also enforced at the worker level. The admission layer delegates the check
// there because it requires a scan of volumes.source_snapshot_id, which is
// safe to do at both layers (worker is authoritative; API is a fast-fail hint).
func (s *server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	snap, ok := s.loadOwnedSnapshot(w, r, principal, id)
	if !ok {
		return
	}

	// Must be in a stable, non-transitional state.
	if isSnapshotTransitional(snap.Status) {
		writeAPIError(w, http.StatusConflict, errSnapshotInvalidState,
			"Cannot delete a snapshot in transitional state '"+snap.Status+"'.", "status")
		return
	}
	if snap.Status != SnapshotStatusAvailable && snap.Status != SnapshotStatusError {
		writeAPIError(w, http.StatusConflict, errSnapshotInvalidState,
			"Cannot delete a snapshot in state '"+snap.Status+"'.", "status")
		return
	}

	// Double-enqueue guard.
	active, err := s.repo.HasActiveSnapshotJob(r.Context(), snap.ID, jobTypeSnapshotDelete)
	if err != nil {
		s.log.Error("HasActiveSnapshotJob (delete) failed", "error", err)
		writeDBError(w, err)
		return
	}
	if active {
		writeAPIError(w, http.StatusConflict, errSnapshotInvalidState,
			"A delete operation is already in progress for this snapshot.", "status")
		return
	}

	jobID := idgen.New(idgen.PrefixJob)
	snapID := snap.ID
	jobRow := &db.JobRow{
		ID:          jobID,
		SnapshotID:  &snapID,
		JobType:     jobTypeSnapshotDelete,
		MaxAttempts: snapshotJobMaxAttempts[jobTypeSnapshotDelete],
	}
	if err := s.repo.InsertSnapshotJob(r.Context(), jobRow); err != nil {
		s.log.Error("InsertSnapshotJob (delete) failed", "error", err)
		writeDBError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, SnapshotLifecycleResponse{
		SnapshotID: snap.ID,
		JobID:      jobID,
		Action:     "delete",
	})
}

// ── POST /v1/snapshots/{id}/restore ──────────────────────────────────────────

// handleRestoreSnapshot handles POST /v1/snapshots/{id}/restore.
// Creates a new volume populated from the snapshot and enqueues a
// VOLUME_RESTORE job. Returns 202 + RestoreSnapshotResponse.
//
// The restored volume starts in 'creating' status. The VOLUME_RESTORE worker
// transitions it to 'available' once storage is ready.
//
// VM-P2B-S3: returns RestoreSnapshotResponse{Volume, JobID} — the full volume
// resource is embedded so callers do not need a separate GET.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (restore flow), §4 (API endpoints),
//
//	vm-15-02__blueprint__ §interaction_or_ops_contract.
func (s *server) handleRestoreSnapshot(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	var req RestoreSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"Request body is not valid JSON.", "")
		return
	}
	if errs := validateRestoreSnapshotRequest(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	snap, ok := s.loadOwnedSnapshot(w, r, principal, id)
	if !ok {
		return
	}

	// Snapshot must be available. SNAP-I-1.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-1.
	if snap.Status != SnapshotStatusAvailable {
		writeAPIError(w, http.StatusConflict, errSnapshotNotAvailable,
			"Snapshot must be in 'available' state to restore. Current state: '"+snap.Status+"'.", "id")
		return
	}

	// Determine restore size: default to snapshot size; override must be >=.
	sizeGB := snap.SizeGB
	if req.SizeGB != nil {
		if *req.SizeGB < snap.SizeGB {
			writeAPIError(w, http.StatusUnprocessableEntity, errRestoreSizeTooSmall,
				"Restore size_gb must be at least the snapshot size_gb ("+
					itoa(snap.SizeGB)+" GB).", "size_gb")
			return
		}
		sizeGB = *req.SizeGB
	}

	// Create the destination volume in 'creating' status.
	// origin = 'snapshot', source_snapshot_id set.
	// Source: P2_VOLUME_MODEL.md §2.1 (VolumeOriginSnapshot).
	volID := idgen.New("vol")
	snapID := snap.ID
	volRow := &db.VolumeRow{
		ID:               volID,
		OwnerPrincipalID: principal,
		DisplayName:      req.Name,
		Region:           s.region,
		AvailabilityZone: req.AvailabilityZone,
		SizeGB:           sizeGB,
		Origin:           "snapshot",
		SourceSnapshotID: &snapID,
	}
	if err := s.repo.CreateVolume(r.Context(), volRow); err != nil {
		s.log.Error("CreateVolume (restore) failed", "error", err)
		writeDBError(w, err)
		return
	}

	// Enqueue VOLUME_RESTORE job. The worker transitions the volume to 'available'.
	// VolumeID is carried on the job so the worker can find the destination
	// volume directly without scanning.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (restore flow is async).
	jobID := idgen.New(idgen.PrefixJob)
	jobRow := &db.JobRow{
		ID:          jobID,
		SnapshotID:  &snapID,
		VolumeID:    &volID,
		JobType:     jobTypeVolumeRestore,
		MaxAttempts: snapshotJobMaxAttempts[jobTypeVolumeRestore],
	}
	if err := s.repo.InsertSnapshotJob(r.Context(), jobRow); err != nil {
		s.log.Error("InsertSnapshotJob (restore) failed", "error", err)
		writeDBError(w, err)
		return
	}

	// Fetch the newly created volume to build the full response.
	// VM-P2B-S3: return VolumeResponse instead of bare volume_id.
	created, err := s.repo.GetVolumeByID(r.Context(), volID)
	if err != nil || created == nil {
		s.log.Error("GetVolumeByID after restore insert failed", "error", err)
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusAccepted, RestoreSnapshotResponse{
		Volume: volumeToResponse(created, nil),
		JobID:  jobID,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// loadOwnedSnapshot fetches a snapshot by ID and enforces ownership.
// Returns (row, true) when the snapshot exists and is owned by principal.
// Returns (nil, false) and writes 404 for not-found or cross-account access.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) loadOwnedSnapshot(w http.ResponseWriter, r *http.Request, principal, id string) (*db.SnapshotRow, bool) {
	row, err := s.repo.GetSnapshotByID(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			writeAPIError(w, http.StatusNotFound, errSnapshotNotFound,
				"The snapshot does not exist or you do not have access to it.", "id")
			return nil, false
		}
		s.log.Error("GetSnapshotByID failed", "error", err)
		writeDBError(w, err)
		return nil, false
	}
	if row == nil {
		writeAPIError(w, http.StatusNotFound, errSnapshotNotFound,
			"The snapshot does not exist or you do not have access to it.", "id")
		return nil, false
	}
	// Ownership check: 404 on mismatch — never 403.
	if row.OwnerPrincipalID != principal {
		writeAPIError(w, http.StatusNotFound, errSnapshotNotFound,
			"The snapshot does not exist or you do not have access to it.", "id")
		return nil, false
	}
	return row, true
}

// snapshotToResponse maps a db.SnapshotRow to the canonical SnapshotResponse.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.3.
func snapshotToResponse(row *db.SnapshotRow) SnapshotResponse {
	return SnapshotResponse{
		ID:               row.ID,
		Name:             row.DisplayName,
		Region:           row.Region,
		SourceVolumeID:   row.SourceVolumeID,
		SourceInstanceID: row.SourceInstanceID,
		SizeGB:           row.SizeGB,
		Status:           row.Status,
		ProgressPercent:  row.ProgressPercent,
		Encrypted:        row.Encrypted,
		CreatedAt:        row.CreatedAt,
		CompletedAt:      row.CompletedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

// isSnapshotTransitional reports whether a snapshot status is in a transitional
// state that blocks new state-mutating operations.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5.
func isSnapshotTransitional(status string) bool {
	switch status {
	case SnapshotStatusPending, SnapshotStatusCreating, SnapshotStatusDeleting:
		return true
	}
	return false
}

// SnapshotStatus string constants used by handlers — mirror db constants
// to avoid importing the domain package from resource-manager.
const (
	SnapshotStatusPending   = "pending"
	SnapshotStatusCreating  = "creating"
	SnapshotStatusAvailable = "available"
	SnapshotStatusError     = "error"
	SnapshotStatusDeleting  = "deleting"
	SnapshotStatusDeleted   = "deleted"
)

// itoa converts an int to string without importing strconv directly in handlers.
// Kept here rather than in a shared util to avoid scope creep.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
