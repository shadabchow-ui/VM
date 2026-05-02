package handlers

// volume_create.go — VOLUME_CREATE job handler.
//
// Source: P2_VOLUME_MODEL.md §3.2 (state machine: creating → available),
//         §5 (storage path assignment), §7 (VOL-I-5 mutex lock),
//         vm-15-01__skill__independent-block-volume-architecture.md.
//         VM-P2B-S3.
//
// Create sequence:
//  1. DB: load volume; if already available → idempotent no-op.
//  2. Validate source state is creating.
//  3. DB: LockVolume — sets locked_by=jobID, version++.
//     Skip if locked_by is already set to this job (re-entrant delivery).
//  4. Storage: provision the blank block device (Phase 2: simulated —
//     derives storage_path and persists it via SetVolumeStoragePath).
//  5. DB: UnlockVolume → available (clears locked_by, status=available,
//     storage_path set in step 4, version++).
//  6. On any failure after lock acquired: UnlockVolume → error.
//
// Idempotency:
//   - volume already available → immediate no-op.
//   - re-entrant (locked_by == job.ID) → skip LockVolume, continue from step 4.
//
// Invariants:
//   VOL-I-5 (P2_VOLUME_MODEL.md §7): at most one mutation lock per volume.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	runtime "github.com/compute-platform/compute-platform/services/host-agent/runtime"
)

// VolumeCreateHandler handles VOLUME_CREATE jobs.
type VolumeCreateHandler struct {
	deps *VolumeDeps
	log  *slog.Logger
}

// NewVolumeCreateHandler constructs a VolumeCreateHandler.
func NewVolumeCreateHandler(deps *VolumeDeps, log *slog.Logger) *VolumeCreateHandler {
	return &VolumeCreateHandler{deps: deps, log: log}
}

// Execute runs the full volume creation sequence. Idempotent on duplicate delivery.
func (h *VolumeCreateHandler) Execute(ctx context.Context, job *db.JobRow) error {
	if job.VolumeID == nil {
		return fmt.Errorf("VOLUME_CREATE: job %s has nil VolumeID", job.ID)
	}
	volumeID := *job.VolumeID
	log := h.log.With("job_id", job.ID, "volume_id", volumeID)
	log.Info("VOLUME_CREATE: starting")

	// ── Step 1: Load volume ───────────────────────────────────────────────────
	vol, err := h.deps.Store.GetVolumeByID(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("step1 load volume: %w", err)
	}
	if vol == nil {
		return fmt.Errorf("VOLUME_CREATE: volume %s not found", volumeID)
	}

	// Idempotent: prior delivery already completed.
	if vol.Status == db.VolumeStatusAvailable {
		log.Info("VOLUME_CREATE: volume already available — idempotent no-op")
		return nil
	}

	// ── Step 2: Validate source state ─────────────────────────────────────────
	if vol.Status != db.VolumeStatusCreating {
		return fmt.Errorf("VOLUME_CREATE: illegal source state %q — expected creating", vol.Status)
	}

	// Re-entrant: prior partial execution locked this job's ID already.
	reentrant := vol.LockedBy != nil && *vol.LockedBy == job.ID

	// ── Step 3: Lock volume ───────────────────────────────────────────────────
	if !reentrant {
		if err := h.deps.Store.LockVolume(ctx, volumeID, job.ID, db.VolumeStatusCreating, vol.Version); err != nil {
			return fmt.Errorf("step3 lock volume: %w", err)
		}
		vol.Version++
		log.Info("step3: volume locked")
	} else {
		log.Info("step3: volume already locked by this job — skip (re-entrant)")
	}

	// ── Step 4: Storage provisioning ──────────────────────────────────────────
	// Derive deterministic safe storage path under the configured local root.
	// If Storage is nil (tests without manager), fall back to legacy path.
	storagePath := deriveVolumeStoragePath(h.deps.Storage, volumeID)
	if err := h.deps.Store.SetVolumeStoragePath(ctx, volumeID, storagePath); err != nil {
		_ = h.unlock(ctx, volumeID, db.VolumeStatusError)
		return fmt.Errorf("step4 set storage path: %w", err)
	}
	log.Info("step4: storage path set", "storage_path", storagePath)

	// ── Step 5: Unlock → available ────────────────────────────────────────────
	// UnlockVolume: clears locked_by, sets status=available, increments version.
	if err := h.unlock(ctx, volumeID, db.VolumeStatusAvailable); err != nil {
		return fmt.Errorf("step5 unlock to available: %w", err)
	}
	log.Info("VOLUME_CREATE: completed", "storage_path", storagePath)
	return nil
}

// unlock calls UnlockVolume; logs on failure so callers can return their own error.
func (h *VolumeCreateHandler) unlock(ctx context.Context, volumeID, newStatus string) error {
	if err := h.deps.Store.UnlockVolume(ctx, volumeID, newStatus); err != nil {
		h.log.Error("UnlockVolume failed",
			"volume_id", volumeID,
			"new_status", newStatus,
			"error", err,
		)
		return err
	}
	return nil
}

// deriveVolumeStoragePath returns the control-plane storage path for a volume.
// Uses the LocalStorageManager when available for safe, configurable path derivation.
// Falls back to a deterministic legacy path when manager is nil (test compatibility).
// Source: vm-15-01__blueprint__ §data_model, VM Job 4.
func deriveVolumeStoragePath(mgr *runtime.LocalStorageManager, volumeID string) string {
	if mgr != nil {
		return mgr.VolumeDiskPath(volumeID)
	}
	return "/volumes/" + volumeID + "/disk.img"
}
