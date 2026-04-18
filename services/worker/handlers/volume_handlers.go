package handlers

// volume_handlers.go — Worker job handlers for volume lifecycle operations.
//
// VM-P2B Slice 1: VOLUME_ATTACH, VOLUME_DETACH, VOLUME_DELETE handlers.
// VM-P2B-S3: VOLUME_CREATE handler added (see volume_create_handler.go or
//   wherever VolumeCreateHandler is defined in this package).
//
// Each handler follows the same pattern established by VolumeCreateHandler:
//   1. Validate job has a non-nil VolumeID.
//   2. Fetch current volume state via VolumeStore.GetVolumeByID.
//   3. Check for idempotency (already in terminal/target state → no-op).
//   4. Validate the volume is in a state that permits this operation.
//   5. Check for re-entrancy (job already locked this volume → skip lock).
//   6. Lock the volume with LockVolume (CAS on status + version).
//   7. Execute the operation (transition status, close attachment, etc.).
//   8. On success: unlock to terminal status.
//   9. On failure: unlock to error status.
//
// VolumeStore interface (defined alongside VolumeCreateHandler):
//   GetVolumeByID(ctx, id) (*db.VolumeRow, error)
//   LockVolume(ctx, id, jobID, expectedStatus string, version int) error
//   UnlockVolume(ctx, id, newStatus string) error
//   UpdateVolumeStatus(ctx, id, expectedStatus, newStatus string, version int) error
//   SoftDeleteVolume(ctx, id string, version int) error
//   GetActiveAttachmentByVolume(ctx, volumeID string) (*db.VolumeAttachmentRow, error)
//   CloseVolumeAttachment(ctx, attachmentID string) error
//   SetVolumeStoragePath(ctx, id, storagePath string) error
//   CountActiveSnapshotsByVolume(ctx, volumeID string) (int, error)
//
// Source: P2_VOLUME_MODEL.md §3 (state machine), §4 (attach/detach flows),
//         §5 (delete), §7 (invariants VOL-I-1, VOL-I-5).

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── VolumeAttachHandler ───────────────────────────────────────────────────────

// VolumeAttachHandler executes VOLUME_ATTACH jobs.
//
// State machine:
//   available → (lock → attaching) → in_use     [success]
//   available → (lock → attaching) → error       [GetActiveAttachmentByVolume fails]
//   available → (lock → attaching) → available   [no attachment row found]
//   in_use                                        [idempotent no-op]
//   attaching (locked by this job)  → in_use      [re-entrant resume]
//
// Source: P2_VOLUME_MODEL.md §4.2 (attach flow), §3.2 (state machine).
type VolumeAttachHandler struct {
	store VolumeStore
	log   *slog.Logger
}

// NewVolumeAttachHandler constructs a VolumeAttachHandler.
func NewVolumeAttachHandler(deps *VolumeDeps, log *slog.Logger) *VolumeAttachHandler {
	return &VolumeAttachHandler{store: deps.Store, log: log}
}

// Execute runs the VOLUME_ATTACH job.
func (h *VolumeAttachHandler) Execute(ctx context.Context, job *db.JobRow) error {
	if job.VolumeID == nil {
		return fmt.Errorf("VolumeAttachHandler: job %s has nil VolumeID", job.ID)
	}
	volID := *job.VolumeID

	vol, err := h.store.GetVolumeByID(ctx, volID)
	if err != nil {
		return fmt.Errorf("VolumeAttachHandler: GetVolumeByID %s: %w", volID, err)
	}
	if vol == nil {
		return fmt.Errorf("VolumeAttachHandler: volume %s not found", volID)
	}

	// Idempotency: already in_use → success no-op.
	// Source: P2_VOLUME_MODEL.md §4.2 (idempotent attach).
	if vol.Status == db.VolumeStatusInUse {
		h.log.Info("VolumeAttachHandler: volume already in_use — no-op", "volume_id", volID, "job_id", job.ID)
		return nil
	}

	// Re-entrancy: if this job already holds the lock (status=attaching, LockedBy==job.ID),
	// skip the LockVolume call and proceed to complete the transition.
	reentrant := vol.Status == db.VolumeStatusAttaching && vol.LockedBy != nil && *vol.LockedBy == job.ID

	if !reentrant {
		// Guard: only 'available' volumes may be attached.
		if vol.Status != db.VolumeStatusAvailable {
			return fmt.Errorf("VolumeAttachHandler: volume %s is in illegal state %q for attach", volID, vol.Status)
		}

		// Lock: transition available → attaching under CAS.
		if err := h.store.LockVolume(ctx, volID, job.ID, db.VolumeStatusAvailable, vol.Version); err != nil {
			return fmt.Errorf("VolumeAttachHandler: LockVolume %s: %w", volID, err)
		}
	}

	// Fetch the active attachment record created at admission time by the handler.
	// Source: P2_VOLUME_MODEL.md §4.2 step 2 (attachment row created before job enqueue).
	att, err := h.store.GetActiveAttachmentByVolume(ctx, volID)
	if err != nil {
		// DB error — unlock to error state so the volume is recoverable.
		_ = h.store.UnlockVolume(ctx, volID, db.VolumeStatusError)
		return fmt.Errorf("VolumeAttachHandler: GetActiveAttachmentByVolume %s: %w", volID, err)
	}
	if att == nil {
		// No attachment row — roll back to available. The admission handler may
		// have failed to create the row; the operator can retry.
		// Source: P2_VOLUME_MODEL.md §4.2 (rollback on missing attachment).
		_ = h.store.UnlockVolume(ctx, volID, db.VolumeStatusAvailable)
		return fmt.Errorf("VolumeAttachHandler: no active attachment found for volume %s — rolled back to available", volID)
	}

	// Transition to in_use: unlock from attaching to in_use.
	if err := h.store.UnlockVolume(ctx, volID, db.VolumeStatusInUse); err != nil {
		return fmt.Errorf("VolumeAttachHandler: UnlockVolume (in_use) %s: %w", volID, err)
	}

	h.log.Info("VolumeAttachHandler: volume attached", "volume_id", volID, "instance_id", att.InstanceID, "job_id", job.ID)
	return nil
}

// ── VolumeDetachHandler ───────────────────────────────────────────────────────

// VolumeDetachHandler executes VOLUME_DETACH jobs.
//
// State machine:
//   in_use    → (lock → detaching) → available   [success: attachment closed]
//   available                                      [idempotent no-op]
//   detaching (locked by this job) → available    [re-entrant resume]
//   detaching (locked by this job, att already closed) → available [re-entrant]
//
// Source: P2_VOLUME_MODEL.md §4.4 (detach flow), §3.2 (state machine).
type VolumeDetachHandler struct {
	store VolumeStore
	log   *slog.Logger
}

// NewVolumeDetachHandler constructs a VolumeDetachHandler.
func NewVolumeDetachHandler(deps *VolumeDeps, log *slog.Logger) *VolumeDetachHandler {
	return &VolumeDetachHandler{store: deps.Store, log: log}
}

// Execute runs the VOLUME_DETACH job.
func (h *VolumeDetachHandler) Execute(ctx context.Context, job *db.JobRow) error {
	if job.VolumeID == nil {
		return fmt.Errorf("VolumeDetachHandler: job %s has nil VolumeID", job.ID)
	}
	volID := *job.VolumeID

	vol, err := h.store.GetVolumeByID(ctx, volID)
	if err != nil {
		return fmt.Errorf("VolumeDetachHandler: GetVolumeByID %s: %w", volID, err)
	}
	if vol == nil {
		return fmt.Errorf("VolumeDetachHandler: volume %s not found", volID)
	}

	// Idempotency: already available → success no-op.
	// Source: P2_VOLUME_MODEL.md §4.4 (idempotent detach).
	if vol.Status == db.VolumeStatusAvailable {
		h.log.Info("VolumeDetachHandler: volume already available — no-op", "volume_id", volID, "job_id", job.ID)
		return nil
	}

	// Re-entrancy: this job already holds the lock (status=detaching, LockedBy==job.ID).
	reentrant := vol.Status == db.VolumeStatusDetaching && vol.LockedBy != nil && *vol.LockedBy == job.ID

	if !reentrant {
		// Guard: only 'in_use' volumes may be detached.
		if vol.Status != db.VolumeStatusInUse {
			return fmt.Errorf("VolumeDetachHandler: volume %s is in illegal state %q for detach", volID, vol.Status)
		}

		// Lock: transition in_use → detaching under CAS.
		if err := h.store.LockVolume(ctx, volID, job.ID, db.VolumeStatusInUse, vol.Version); err != nil {
			return fmt.Errorf("VolumeDetachHandler: LockVolume %s: %w", volID, err)
		}
	}

	// Close the active attachment record.
	// If the attachment was already closed in a prior partial run, GetActiveAttachmentByVolume
	// returns nil — that is also fine; we skip CloseVolumeAttachment and proceed to unlock.
	// Source: P2_VOLUME_MODEL.md §4.4 step 2 (close attachment record).
	att, err := h.store.GetActiveAttachmentByVolume(ctx, volID)
	if err != nil {
		_ = h.store.UnlockVolume(ctx, volID, db.VolumeStatusError)
		return fmt.Errorf("VolumeDetachHandler: GetActiveAttachmentByVolume %s: %w", volID, err)
	}
	if att != nil {
		if err := h.store.CloseVolumeAttachment(ctx, att.ID); err != nil {
			_ = h.store.UnlockVolume(ctx, volID, db.VolumeStatusError)
			return fmt.Errorf("VolumeDetachHandler: CloseVolumeAttachment %s: %w", att.ID, err)
		}
	}

	// Transition to available: unlock from detaching to available.
	if err := h.store.UnlockVolume(ctx, volID, db.VolumeStatusAvailable); err != nil {
		return fmt.Errorf("VolumeDetachHandler: UnlockVolume (available) %s: %w", volID, err)
	}

	h.log.Info("VolumeDetachHandler: volume detached", "volume_id", volID, "job_id", job.ID)
	return nil
}

// ── VolumeDeleteHandler ───────────────────────────────────────────────────────

// VolumeDeleteHandler executes VOLUME_DELETE jobs.
//
// State machine:
//   available → (lock → deleting) → deleted      [success]
//   available → (lock → deleting) → error        [SoftDeleteVolume fails]
//   deleted                                       [idempotent no-op]
//   (not found)                                   [idempotent no-op — ghost job]
//   deleting (locked by this job) → deleted      [re-entrant resume]
//   in_use                                        [error — VOL-SM-1]
//   creating                                      [error — transitional]
//
// Source: P2_VOLUME_MODEL.md §5.2 (delete flow), §3.3 VOL-SM-1, §7 VOL-I-5.
type VolumeDeleteHandler struct {
	store VolumeStore
	log   *slog.Logger
}

// NewVolumeDeleteHandler constructs a VolumeDeleteHandler.
func NewVolumeDeleteHandler(deps *VolumeDeps, log *slog.Logger) *VolumeDeleteHandler {
	return &VolumeDeleteHandler{store: deps.Store, log: log}
}

// Execute runs the VOLUME_DELETE job.
func (h *VolumeDeleteHandler) Execute(ctx context.Context, job *db.JobRow) error {
	if job.VolumeID == nil {
		return fmt.Errorf("VolumeDeleteHandler: job %s has nil VolumeID", job.ID)
	}
	volID := *job.VolumeID

	vol, err := h.store.GetVolumeByID(ctx, volID)
	if err != nil {
		return fmt.Errorf("VolumeDeleteHandler: GetVolumeByID %s: %w", volID, err)
	}

	// Idempotency: volume not found or already deleted → success no-op.
	// Source: P2_VOLUME_MODEL.md §5.2 (delete is idempotent).
	if vol == nil {
		h.log.Info("VolumeDeleteHandler: volume not found — no-op", "volume_id", volID, "job_id", job.ID)
		return nil
	}
	if vol.Status == db.VolumeStatusDeleted {
		h.log.Info("VolumeDeleteHandler: volume already deleted — no-op", "volume_id", volID, "job_id", job.ID)
		return nil
	}

	// Re-entrancy: this job already holds the lock (status=deleting, LockedBy==job.ID).
	reentrant := vol.Status == db.VolumeStatusDeleting && vol.LockedBy != nil && *vol.LockedBy == job.ID

	if !reentrant {
		// Guard: VOL-SM-1 — in_use volumes must not be deleted.
		// Source: P2_VOLUME_MODEL.md §3.3 VOL-SM-1.
		if vol.Status == db.VolumeStatusInUse {
			return fmt.Errorf("VolumeDeleteHandler: volume %s is in_use — cannot delete", volID)
		}

		// Guard: block deletion from transitional states (creating, attaching, detaching).
		// Source: P2_VOLUME_MODEL.md §3.3 VOL-SM-2.
		if vol.Status != db.VolumeStatusAvailable {
			return fmt.Errorf("VolumeDeleteHandler: volume %s is in state %q — only available volumes may be deleted", volID, vol.Status)
		}

		// Lock: transition available → deleting under CAS.
		if err := h.store.LockVolume(ctx, volID, job.ID, db.VolumeStatusAvailable, vol.Version); err != nil {
			return fmt.Errorf("VolumeDeleteHandler: LockVolume %s: %w", volID, err)
		}
	}

	// Re-fetch version after lock (version incremented by LockVolume).
	// For re-entrant path, use the version already on the fetched row.
	locked, err := h.store.GetVolumeByID(ctx, volID)
	if err != nil || locked == nil {
		_ = h.store.UnlockVolume(ctx, volID, db.VolumeStatusError)
		return fmt.Errorf("VolumeDeleteHandler: GetVolumeByID after lock %s: %w", volID, err)
	}

	// Soft-delete: mark the volume as deleted in the DB.
	// Source: P2_VOLUME_MODEL.md §5.2 step 2.
	if err := h.store.SoftDeleteVolume(ctx, volID, locked.Version); err != nil {
		_ = h.store.UnlockVolume(ctx, volID, db.VolumeStatusError)
		return fmt.Errorf("VolumeDeleteHandler: SoftDeleteVolume %s: %w", volID, err)
	}

	h.log.Info("VolumeDeleteHandler: volume deleted", "volume_id", volID, "job_id", job.ID)
	return nil
}
