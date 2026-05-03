package main

// volume_handlers.go — HTTP handlers for the public volume management API.
//
// VM-P2B Slice 1: independent block volume CRUD and attach/detach.
// VM-P2B-S3:
//   - handleDeleteVolume: enforce SNAP-I-3 (reject if active snapshots exist).
//   - volumeToResponse: include SourceSnapshotID for origin=snapshot volumes.
// VM-P3A repair:
//   - handleInstanceVolumeSubroute: dispatcher for /v1/instances/{id}/volumes[/{volume_id}]
//     called by instance_handlers.go. Previously missing — caused failure 1.
//
// Routes registered:
//   POST   /v1/volumes                              → handleCreateVolume      (202 + Job)
//   GET    /v1/volumes                              → handleListVolumes       (200)
//   GET    /v1/volumes/{id}                         → handleGetVolume         (200)
//   DELETE /v1/volumes/{id}                         → handleDeleteVolume      (202 + Job)
//   GET    /v1/instances/{id}/volumes               → handleListInstanceVolumes (200)
//   POST   /v1/instances/{id}/volumes               → handleAttachVolume      (202 + Job)
//   DELETE /v1/instances/{id}/volumes/{volume_id}   → handleDetachVolume      (202 + Job)
//
// Design rules (same as instance_handlers.go):
//   - Handlers never call runtime directly.
//   - All mutating operations enqueue a job and return 202.
//   - Ownership enforced via loadOwnedVolume (404-for-cross-account).
//   - DB errors flow through writeDBError (503/500 per DB-6 gate).
//   - Validation uses fieldErr / writeAPIErrors (no new framework).
//
// Source: P2_VOLUME_MODEL.md §4 (attach/detach flows), §5 (delete),
//         §8 (API endpoint summary), JOB_MODEL_V1, AUTH_OWNERSHIP_MODEL_V1 §3,
//         API_ERROR_CONTRACT_V1 §1, §4.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// Volume job type constants — used by handlers to enqueue jobs.
// Source: P2_VOLUME_MODEL.md §4.2, §4.4, §5.2.
const (
	jobTypeVolumeCreate = "VOLUME_CREATE"
	jobTypeVolumeDelete = "VOLUME_DELETE"
	jobTypeVolumeAttach = "VOLUME_ATTACH"
	jobTypeVolumeDetach = "VOLUME_DETACH"
)

// volumeJobMaxAttempts per volume job type.
// Source: P2_VOLUME_MODEL.md §4.2 (worker retry semantics).
var volumeJobMaxAttempts = map[string]int{
	jobTypeVolumeCreate: 3,
	jobTypeVolumeDelete: 5,
	jobTypeVolumeAttach: 5,
	jobTypeVolumeDetach: 5,
}

// maxVolumesPerInstance is the Phase 2 limit on attached volumes per instance.
// Source: P2_VOLUME_MODEL.md §4.1.
const maxVolumesPerInstance = 16

// ── Route registration ────────────────────────────────────────────────────────

// registerVolumeRoutes registers the public volume API routes.
// The instance sub-routes (/v1/instances/{id}/volumes) are integrated into
// handleInstanceByID via registerInstanceVolumeSubroutes, called from
// the existing instance route handler after this method adds the top-level routes.
func (s *server) registerVolumeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/volumes", requirePrincipal(s.handleVolumeRoot))
	mux.HandleFunc("/v1/volumes/", requirePrincipal(s.handleVolumeByID))
}

// handleVolumeRoot dispatches POST /v1/volumes and GET /v1/volumes.
func (s *server) handleVolumeRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateVolume(w, r)
	case http.MethodGet:
		s.handleListVolumes(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVolumeByID dispatches based on method for /v1/volumes/{id}.
func (s *server) handleVolumeByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/volumes/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	id := strings.SplitN(rest, "/", 2)[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetVolume(w, r, id)
	case http.MethodDelete:
		s.handleDeleteVolume(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── /v1/instances/{id}/volumes sub-route dispatcher ──────────────────────────

// handleInstanceVolumeSubroute dispatches requests for the volume sub-routes
// nested under /v1/instances/{id}/volumes[/{volume_id}].
//
// Called by handleInstanceByID in instance_handlers.go when subpath == "volumes".
// parts is the same SplitN(rest, "/", 3) slice used by handleInstanceByID:
//
//	parts[0] = instance ID (already extracted by caller as id)
//	parts[1] = "volumes"
//	parts[2] = volume_id (only present for DELETE)
//
// Dispatch table:
//
//	GET    /v1/instances/{id}/volumes              → handleListInstanceVolumes
//	POST   /v1/instances/{id}/volumes              → handleAttachVolume
//	DELETE /v1/instances/{id}/volumes/{volume_id}  → handleDetachVolume
//
// Source: P2_VOLUME_MODEL.md §8 (API endpoint summary).
func (s *server) handleInstanceVolumeSubroute(w http.ResponseWriter, r *http.Request, instanceID string, parts []string) {
	// parts[2] is the volume_id segment (only present when len(parts) == 3).
	hasVolumeID := len(parts) >= 3 && parts[2] != ""

	switch r.Method {
	case http.MethodGet:
		if hasVolumeID {
			// GET /v1/instances/{id}/volumes/{volume_id} — not a defined endpoint.
			http.NotFound(w, r)
			return
		}
		s.handleListInstanceVolumes(w, r, instanceID)

	case http.MethodPost:
		if hasVolumeID {
			http.NotFound(w, r)
			return
		}
		s.handleAttachVolume(w, r, instanceID)

	case http.MethodDelete:
		if !hasVolumeID {
			// DELETE /v1/instances/{id}/volumes with no volume_id — not allowed.
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleDetachVolume(w, r, instanceID, parts[2])

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── POST /v1/volumes ──────────────────────────────────────────────────────────

// handleCreateVolume handles POST /v1/volumes.
// Creates a blank volume and enqueues a VOLUME_CREATE job.
// Returns 202 Accepted + CreateVolumeResponse.
// Source: P2_VOLUME_MODEL.md §3.2 (initial state: creating), §8.
func (s *server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	var req CreateVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"Request body is not valid JSON.", "")
		return
	}

	if errs := validateCreateVolumeRequest(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	// Quota admission: check before inserting the volume row or enqueuing a job.
	// The quota scope is the calling principal (classic/no-project model).
	// db.ErrQuotaExceeded → 422 quota_exceeded. Distinct from capacity errors.
	// Source: vm-13-02__blueprint__ §core_contracts "Error Code Separation".
	if err := s.repo.CheckVolumeCreateQuota(r.Context(), principal, req.SizeGB); err != nil {
		if errors.Is(err, db.ErrQuotaExceeded) {
			writeAPIError(w, http.StatusUnprocessableEntity, errQuotaExceeded,
				"Volume storage quota exceeded: "+err.Error(),
				"size_gb")
			return
		}
		s.log.Error("CheckVolumeCreateQuota failed", "principal", principal, "error", err)
		writeDBError(w, err)
		return
	}

	volumeID := idgen.New("vol")

	row := &db.VolumeRow{
		ID:               volumeID,
		OwnerPrincipalID: principal,
		DisplayName:      req.Name,
		Region:           s.region,
		AvailabilityZone: req.AvailabilityZone,
		SizeGB:           req.SizeGB,
		Origin:           "blank",
	}

	if err := s.repo.CreateVolume(r.Context(), row); err != nil {
		s.log.Error("CreateVolume failed", "error", err)
		writeDBError(w, err)
		return
	}

	jobID := idgen.New(idgen.PrefixJob)
	jobRow := &db.JobRow{
		ID:          jobID,
		VolumeID:    &volumeID,
		JobType:     jobTypeVolumeCreate,
		MaxAttempts: volumeJobMaxAttempts[jobTypeVolumeCreate],
	}
	if err := s.repo.InsertVolumeJob(r.Context(), jobRow); err != nil {
		s.log.Error("InsertVolumeJob (create) failed", "error", err)
		writeDBError(w, err)
		return
	}

	created, err := s.repo.GetVolumeByID(r.Context(), volumeID)
	if err != nil || created == nil {
		s.log.Error("GetVolumeByID after insert failed", "error", err)
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusAccepted, CreateVolumeResponse{
		Volume: volumeToResponse(created, nil),
		JobID:  jobID,
	})
}

// ── GET /v1/volumes ───────────────────────────────────────────────────────────

// handleListVolumes handles GET /v1/volumes.
// Returns 200 + ListVolumesResponse scoped to the calling principal.
// Source: P2_VOLUME_MODEL.md §8, AUTH_OWNERSHIP_MODEL_V1 §4.
func (s *server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	rows, err := s.repo.ListVolumesByOwner(r.Context(), principal)
	if err != nil {
		s.log.Error("ListVolumesByOwner failed", "error", err)
		writeDBError(w, err)
		return
	}

	out := make([]VolumeResponse, 0, len(rows))
	for _, row := range rows {
		att, _ := s.repo.GetActiveAttachmentByVolume(r.Context(), row.ID)
		out = append(out, volumeToResponse(row, att))
	}

	writeJSON(w, http.StatusOK, ListVolumesResponse{
		Volumes: out,
		Total:   len(out),
	})
}

// ── GET /v1/volumes/{id} ──────────────────────────────────────────────────────

// handleGetVolume handles GET /v1/volumes/{id}.
// Returns 200 + VolumeResponse or 404.
// Source: P2_VOLUME_MODEL.md §8, AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) handleGetVolume(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	vol, ok := s.loadOwnedVolume(w, r, principal, id)
	if !ok {
		return
	}

	att, _ := s.repo.GetActiveAttachmentByVolume(r.Context(), vol.ID)
	writeJSON(w, http.StatusOK, volumeToResponse(vol, att))
}

// ── DELETE /v1/volumes/{id} ───────────────────────────────────────────────────

// handleDeleteVolume handles DELETE /v1/volumes/{id}.
// Returns 202 + VolumeLifecycleResponse or 409 when volume is in_use,
// transitional, or has active snapshots.
//
// Admission checks:
//   - VOL-SM-1: cannot delete in_use volume.
//   - VOL-SM-2: cannot delete a volume in a transitional state.
//   - SNAP-I-3: cannot delete a volume that has non-deleted snapshots.
//
// Source: P2_VOLUME_MODEL.md §5.2, §3.3 VOL-SM-1, §7 VOL-I-4,
//
//	P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.
func (s *server) handleDeleteVolume(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	vol, ok := s.loadOwnedVolume(w, r, principal, id)
	if !ok {
		return
	}

	// VOL-SM-1: cannot delete a volume that is in_use.
	// Source: P2_VOLUME_MODEL.md §3.3 VOL-SM-1, §7 VOL-I-4.
	if vol.Status == "in_use" {
		writeAPIError(w, http.StatusConflict, errVolumeInUse,
			"Cannot delete a volume that is currently attached to an instance.", "status")
		return
	}

	// VOL-SM-2: cannot delete while in a transitional state.
	// Source: P2_VOLUME_MODEL.md §3.3 VOL-SM-2.
	if isVolumeTransitional(vol.Status) {
		writeAPIError(w, http.StatusConflict, errVolumeInvalidState,
			"Cannot delete a volume that is in a transitional state '"+vol.Status+"'.", "status")
		return
	}

	// SNAP-I-3: cannot delete a volume that has active (non-deleted) snapshots.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.
	// VM-P2B-S3.
	snapCount, err := s.repo.CountActiveSnapshotsByVolume(r.Context(), vol.ID)
	if err != nil {
		s.log.Error("CountActiveSnapshotsByVolume failed", "volume_id", vol.ID, "error", err)
		writeDBError(w, err)
		return
	}
	if snapCount > 0 {
		writeAPIError(w, http.StatusConflict, errVolumeHasSnapshots,
			"Cannot delete a volume that has active snapshots. Delete all snapshots of this volume first.", "id")
		return
	}

	jobID := idgen.New(idgen.PrefixJob)
	volID := vol.ID
	jobRow := &db.JobRow{
		ID:          jobID,
		VolumeID:    &volID,
		JobType:     jobTypeVolumeDelete,
		MaxAttempts: volumeJobMaxAttempts[jobTypeVolumeDelete],
	}
	if err := s.repo.InsertVolumeJob(r.Context(), jobRow); err != nil {
		s.log.Error("InsertVolumeJob (delete) failed", "error", err)
		writeDBError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, VolumeLifecycleResponse{
		VolumeID: vol.ID,
		JobID:    jobID,
		Action:   "delete",
	})
}

// ── GET /v1/instances/{id}/volumes ────────────────────────────────────────────

// handleListInstanceVolumes handles GET /v1/instances/{id}/volumes.
// Returns 200 + ListInstanceVolumesResponse.
// Instance ownership is enforced first. Each attachment's volume is fetched.
// Source: P2_VOLUME_MODEL.md §8.
func (s *server) handleListInstanceVolumes(w http.ResponseWriter, r *http.Request, instanceID string) {
	principal, _ := principalFromCtx(r.Context())

	// Step 1: enforce instance ownership.
	_, ok := s.loadOwnedInstance(w, r, principal, instanceID)
	if !ok {
		return
	}

	// Step 2: fetch active attachments for this instance.
	attachments, err := s.repo.ListActiveAttachmentsByInstance(r.Context(), instanceID)
	if err != nil {
		s.log.Error("ListActiveAttachmentsByInstance failed", "instance_id", instanceID, "error", err)
		writeDBError(w, err)
		return
	}

	out := make([]InstanceVolumeEntry, 0, len(attachments))
	for _, att := range attachments {
		vol, err := s.repo.GetVolumeByID(r.Context(), att.VolumeID)
		if err != nil || vol == nil {
			// Volume missing or inaccessible — skip rather than fail the list.
			s.log.Warn("GetVolumeByID failed for attachment", "volume_id", att.VolumeID, "error", err)
			continue
		}
		out = append(out, InstanceVolumeEntry{
			VolumeID:            vol.ID,
			Name:                vol.DisplayName,
			SizeGB:              vol.SizeGB,
			Status:              vol.Status,
			DevicePath:          att.DevicePath,
			DeleteOnTermination: att.DeleteOnTermination,
			AttachedAt:          att.AttachedAt,
		})
	}

	writeJSON(w, http.StatusOK, ListInstanceVolumesResponse{
		Volumes: out,
		Total:   len(out),
	})
}

// ── POST /v1/instances/{id}/volumes ──────────────────────────────────────────

// handleAttachVolume handles POST /v1/instances/{id}/volumes.
// Validates preconditions, enqueues VOLUME_ATTACH job, returns 202.
// Source: P2_VOLUME_MODEL.md §4.2 (attach flow).
func (s *server) handleAttachVolume(w http.ResponseWriter, r *http.Request, instanceID string) {
	principal, _ := principalFromCtx(r.Context())

	// Step 1: validate request body.
	var req AttachVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"Request body is not valid JSON.", "")
		return
	}
	if errs := validateAttachVolumeRequest(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	// Step 2: enforce instance ownership.
	inst, ok := s.loadOwnedInstance(w, r, principal, instanceID)
	if !ok {
		return
	}

	// Step 3: instance must be stopped.
	// Prefer stopped-only attach unless runtime safely supports hotplug.
	// Source: VM Job 4 — Local block-volume persistence (stopped-only attach/detach).
	if inst.VMState != "stopped" {
		writeAPIError(w, http.StatusConflict, errIllegalTransition,
			"Cannot attach a volume to an instance in state '"+inst.VMState+"'. Instance must be stopped.", "status")
		return
	}

	// Step 4: enforce volume ownership (same principal).
	// Source: P2_VOLUME_MODEL.md §4.2 step 2, AUTH_OWNERSHIP_MODEL_V1 §3.
	vol, ok := s.loadOwnedVolume(w, r, principal, req.VolumeID)
	if !ok {
		return
	}

	// Step 5: volume must be available.
	// Source: P2_VOLUME_MODEL.md §3.2, §4.1.
	if vol.Status != "available" {
		if vol.Status == "in_use" {
			writeAPIError(w, http.StatusConflict, errVolumeAlreadyAttached,
				"Volume is already attached to an instance.", "volume_id")
		} else if isVolumeTransitional(vol.Status) {
			writeAPIError(w, http.StatusConflict, errVolumeInvalidState,
				"Cannot attach a volume that is in state '"+vol.Status+"'.", "volume_id")
		} else {
			writeAPIError(w, http.StatusConflict, errVolumeInvalidState,
				"Volume is not in 'available' state.", "volume_id")
		}
		return
	}

	// Step 6: AZ affinity — volume and instance must be in the same AZ.
	// Source: P2_VOLUME_MODEL.md §4.1, §7 VOL-I-2.
	if vol.AvailabilityZone != inst.AvailabilityZone {
		writeAPIError(w, http.StatusUnprocessableEntity, errVolumeAZMismatch,
			"Volume availability zone '"+vol.AvailabilityZone+"' does not match instance availability zone '"+inst.AvailabilityZone+"'.", "volume_id")
		return
	}

	// Step 7: enforce per-instance volume limit.
	// Source: P2_VOLUME_MODEL.md §4.1 (max 16 volumes).
	count, err := s.repo.CountActiveAttachmentsByInstance(r.Context(), instanceID)
	if err != nil {
		s.log.Error("CountActiveAttachmentsByInstance failed", "error", err)
		writeDBError(w, err)
		return
	}
	if count >= maxVolumesPerInstance {
		writeAPIError(w, http.StatusUnprocessableEntity, errVolumeLimitExceeded,
			"Instance has reached the maximum of 16 attached volumes.", "volume_id")
		return
	}

	// Step 8: determine device path.
	// Source: P2_VOLUME_MODEL.md §4.1 (system-assigned if unspecified).
	devicePath := ""
	if req.DevicePath != nil && *req.DevicePath != "" {
		devicePath = *req.DevicePath
	} else {
		devicePath, err = s.repo.NextDevicePath(r.Context(), instanceID)
		if err != nil {
			s.log.Error("NextDevicePath failed", "error", err)
			writeInternalError(w)
			return
		}
	}

	// Step 9: determine delete_on_termination. Default false for data volumes.
	// Source: P2_VOLUME_MODEL.md §5.1.
	deleteOnTermination := false
	if req.DeleteOnTermination != nil {
		deleteOnTermination = *req.DeleteOnTermination
	}

	// Step 10: enqueue VOLUME_ATTACH job.
	// The worker performs the actual host-agent attach and status transitions.
	// Source: P2_VOLUME_MODEL.md §4.2 step 3.
	jobID := idgen.New(idgen.PrefixJob)
	volID := vol.ID
	jobRow := &db.JobRow{
		ID:          jobID,
		VolumeID:    &volID,
		JobType:     jobTypeVolumeAttach,
		MaxAttempts: volumeJobMaxAttempts[jobTypeVolumeAttach],
	}
	if err := s.repo.InsertVolumeJob(r.Context(), jobRow); err != nil {
		s.log.Error("InsertVolumeJob (attach) failed", "error", err)
		writeDBError(w, err)
		return
	}

	// Step 11: create the attachment record with the job context stored in DevicePath.
	// The attachment row is created here at admission time so the worker has a record
	// to update. The volume status remains 'available' until the worker locks it.
	// Note: the unique partial index enforces VOL-I-1 at the DB layer.
	attID := idgen.New("vatt")
	attRow := &db.VolumeAttachmentRow{
		ID:                  attID,
		VolumeID:            vol.ID,
		InstanceID:          instanceID,
		DevicePath:          devicePath,
		DeleteOnTermination: deleteOnTermination,
	}
	if err := s.repo.CreateVolumeAttachment(r.Context(), attRow); err != nil {
		s.log.Error("CreateVolumeAttachment failed", "error", err)
		// If the unique index fires (concurrent attach), surface as 409.
		if isUniqueConstraintErr(err) {
			writeAPIError(w, http.StatusConflict, errVolumeAlreadyAttached,
				"Volume is already attached to an instance.", "volume_id")
		} else {
			writeDBError(w, err)
		}
		return
	}

	writeJSON(w, http.StatusAccepted, VolumeLifecycleResponse{
		VolumeID: vol.ID,
		JobID:    jobID,
		Action:   "attach",
	})
}

// ── DELETE /v1/instances/{id}/volumes/{volume_id} ─────────────────────────────

// handleDetachVolume handles DELETE /v1/instances/{id}/volumes/{volume_id}.
// Validates preconditions, enqueues VOLUME_DETACH job, returns 202.
// Source: P2_VOLUME_MODEL.md §4.4 (detach flow).
func (s *server) handleDetachVolume(w http.ResponseWriter, r *http.Request, instanceID, volumeID string) {
	principal, _ := principalFromCtx(r.Context())

	// Step 1: enforce instance ownership.
	inst, ok := s.loadOwnedInstance(w, r, principal, instanceID)
	if !ok {
		return
	}

	// Step 2: instance must be stopped.
	// Prefer stopped-only detach unless runtime safely supports hotplug.
	// Source: VM Job 4 — Local block-volume persistence (stopped-only attach/detach).
	if inst.VMState != "stopped" {
		writeAPIError(w, http.StatusConflict, errIllegalTransition,
			"Cannot detach a volume from an instance in state '"+inst.VMState+"'. Instance must be stopped.", "status")
		return
	}

	// Step 3: enforce volume ownership.
	vol, ok := s.loadOwnedVolume(w, r, principal, volumeID)
	if !ok {
		return
	}

	// Step 4: verify the volume is actually attached to this instance.
	att, err := s.repo.GetActiveAttachmentByVolume(r.Context(), vol.ID)
	if err != nil {
		s.log.Error("GetActiveAttachmentByVolume failed", "error", err)
		writeDBError(w, err)
		return
	}
	if att == nil || att.InstanceID != instanceID {
		writeAPIError(w, http.StatusConflict, errVolumeNotAttached,
			"Volume is not attached to this instance.", "volume_id")
		return
	}

	// Step 5: enqueue VOLUME_DETACH job.
	// Worker performs host-agent detach and status transitions.
	// Source: P2_VOLUME_MODEL.md §4.4 step 3.
	jobID := idgen.New(idgen.PrefixJob)
	volID := vol.ID
	jobRow := &db.JobRow{
		ID:          jobID,
		VolumeID:    &volID,
		JobType:     jobTypeVolumeDetach,
		MaxAttempts: volumeJobMaxAttempts[jobTypeVolumeDetach],
	}
	if err := s.repo.InsertVolumeJob(r.Context(), jobRow); err != nil {
		s.log.Error("InsertVolumeJob (detach) failed", "error", err)
		writeDBError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, VolumeLifecycleResponse{
		VolumeID: vol.ID,
		JobID:    jobID,
		Action:   "detach",
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// loadOwnedVolume fetches a volume by ID and enforces ownership.
//
// Returns (row, true) when the volume exists and is owned by principal.
// Returns (nil, false) and writes a response for any of:
//   - volume not found → 404
//   - volume owned by different principal → 404 (no existence leak)
//   - transient DB connectivity failure → 503
//   - other DB error → 500
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §3.
// Pattern mirrors loadOwnedInstance from instance_auth.go.
func (s *server) loadOwnedVolume(w http.ResponseWriter, r *http.Request, principal, id string) (*db.VolumeRow, bool) {
	row, err := s.repo.GetVolumeByID(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			writeAPIError(w, http.StatusNotFound, errVolumeNotFound,
				"The volume does not exist or you do not have access to it.", "id")
			return nil, false
		}
		s.log.Error("GetVolumeByID failed", "error", err)
		writeDBError(w, err)
		return nil, false
	}
	if row == nil {
		writeAPIError(w, http.StatusNotFound, errVolumeNotFound,
			"The volume does not exist or you do not have access to it.", "id")
		return nil, false
	}

	// Ownership check: 404 on mismatch — never 403.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	if row.OwnerPrincipalID != principal {
		writeAPIError(w, http.StatusNotFound, errVolumeNotFound,
			"The volume does not exist or you do not have access to it.", "id")
		return nil, false
	}

	return row, true
}

// volumeToResponse maps a db.VolumeRow and optional active attachment to the
// canonical VolumeResponse shape.
// VM-P2B-S3: include SourceSnapshotID for origin=snapshot volumes.
// Source: P2_VOLUME_MODEL.md §2.3.
func volumeToResponse(row *db.VolumeRow, att *db.VolumeAttachmentRow) VolumeResponse {
	resp := VolumeResponse{
		ID:               row.ID,
		Name:             row.DisplayName,
		Region:           row.Region,
		AvailabilityZone: row.AvailabilityZone,
		SizeGB:           row.SizeGB,
		Status:           row.Status,
		Origin:           row.Origin,
		SourceDiskID:     row.SourceDiskID,
		SourceSnapshotID: row.SourceSnapshotID, // VM-P2B-S3: populated for origin=snapshot
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
	if att != nil {
		resp.Attachment = &VolumeAttachmentResponse{
			InstanceID:          att.InstanceID,
			DevicePath:          att.DevicePath,
			DeleteOnTermination: att.DeleteOnTermination,
			AttachedAt:          att.AttachedAt,
		}
	}
	return resp
}

// isVolumeTransitional reports whether a volume status is in a transitional state
// that blocks new state-mutating operations.
// Source: P2_VOLUME_MODEL.md §3.3 VOL-SM-2.
func isVolumeTransitional(status string) bool {
	switch status {
	case "creating", "attaching", "detaching", "deleting":
		return true
	}
	return false
}

// isUniqueConstraintErr reports whether err is a PostgreSQL unique constraint violation.
// Used to detect concurrent attach races caught by idx_volume_attachments_active.
// Source: P2_VOLUME_MODEL.md §7 VOL-I-1.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "idx_volume_attachments_active")
}
