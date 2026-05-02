package handlers

// volume_restore.go — VOLUME_RESTORE job handler.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (restore flow),
//         vm-15-02__blueprint__ §interaction_or_ops_contract,
//         P2_VOLUME_MODEL.md §2.1 (VolumeOriginSnapshot).
//         VM-P2B-S2.
//         VM-P2B-S3: persist derived storage_path via SetVolumeStoragePath before unlock.
//
// Restore sequence:
//  1. DB: load snapshot; validate it is still available (SNAP-I-1).
//  2. DB: find the destination volume via job.VolumeID (set at admission).
//         If already available → idempotent no-op.
//  3. Storage: derive storage_path for the new volume (CoW overlay over snapshot).
//     Persist via SetVolumeStoragePath so the path is recorded before status change.
//  4. DB: UnlockVolume → available (clears locked_by if set, sets status=available).
//  5. On any failure: UnlockVolume → error so the failure is inspectable.
//
// Idempotency:
//   - destination volume already available → no-op.
//   - destination volume not found → error (admission failure; not retriable by default).
//
// Invariants:
//   SNAP-I-1: snapshot must remain available throughout restore — if the snapshot
//             is deleted before the restore completes, the job fails.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	runtime "github.com/compute-platform/compute-platform/services/host-agent/runtime"
)

// VolumeRestoreHandler handles VOLUME_RESTORE jobs.
type VolumeRestoreHandler struct {
	deps *SnapshotDeps
	log  *slog.Logger
}

// NewVolumeRestoreHandler constructs a VolumeRestoreHandler.
func NewVolumeRestoreHandler(deps *SnapshotDeps, log *slog.Logger) *VolumeRestoreHandler {
	return &VolumeRestoreHandler{deps: deps, log: log}
}

// Execute runs the full restore sequence. Idempotent on duplicate delivery.
func (h *VolumeRestoreHandler) Execute(ctx context.Context, job *db.JobRow) error {
	if job.SnapshotID == nil {
		return fmt.Errorf("VOLUME_RESTORE: job %s has nil SnapshotID", job.ID)
	}
	snapID := *job.SnapshotID
	log := h.log.With("job_id", job.ID, "snapshot_id", snapID)
	log.Info("VOLUME_RESTORE: starting")

	// ── Step 1: Verify snapshot is still available ────────────────────────────
	// SNAP-I-1: restore must not complete against a non-available snapshot.
	snap, err := h.deps.Store.GetSnapshotByID(ctx, snapID)
	if err != nil {
		return fmt.Errorf("step1 load snapshot: %w", err)
	}
	if snap == nil {
		return fmt.Errorf("VOLUME_RESTORE: snapshot %s not found — cannot restore", snapID)
	}
	if snap.Status != db.SnapshotStatusAvailable {
		return fmt.Errorf("VOLUME_RESTORE: snapshot %s is in state %q — must be available", snapID, snap.Status)
	}
	if snap.StoragePath == nil {
		return fmt.Errorf("VOLUME_RESTORE: snapshot %s has no storage_path — cannot restore", snapID)
	}
	log.Info("step1: snapshot verified available", "storage_path", *snap.StoragePath)

	// ── Step 2: Find the destination volume ───────────────────────────────────
	// The volume was created at admission by handleRestoreSnapshot with
	// origin='snapshot', source_snapshot_id=snapID, status='creating'.
	// The VOLUME_RESTORE job carries both snapshot_id AND volume_id set by the
	// handler at enqueue time so the worker has a direct reference.
	if job.VolumeID == nil {
		return fmt.Errorf("VOLUME_RESTORE: job %s has nil VolumeID — cannot locate destination volume", job.ID)
	}
	volID := *job.VolumeID

	vol, err := h.deps.Store.GetVolumeByID(ctx, volID)
	if err != nil {
		return fmt.Errorf("step2 load volume: %w", err)
	}
	if vol == nil {
		return fmt.Errorf("VOLUME_RESTORE: destination volume %s not found", volID)
	}

	// Idempotent: prior delivery already completed.
	if vol.Status == db.VolumeStatusAvailable {
		log.Info("VOLUME_RESTORE: volume already available — idempotent no-op")
		return nil
	}

	if vol.Status != db.VolumeStatusCreating {
		return fmt.Errorf("VOLUME_RESTORE: volume %s is in state %q — expected creating", volID, vol.Status)
	}
	log.Info("step2: destination volume found", "volume_id", volID)

	// ── Step 3: Persist storage path ─────────────────────────────────────────
	// Derive deterministic safe storage path under the configured local root.
	// If Storage is nil (tests without manager), fall back to legacy path.
	storagePath := deriveRestoreStoragePath(h.deps.Storage, snapID, volID)
	if err := h.deps.Store.SetVolumeStoragePath(ctx, volID, storagePath); err != nil {
		_ = h.deps.Store.UnlockVolume(ctx, volID, db.VolumeStatusError)
		return fmt.Errorf("step3 set storage path: %w", err)
	}
	log.Info("step3: storage path set", "storage_path", storagePath)

	// ── Step 4: Transition volume creating → available ────────────────────────
	// UnlockVolume: clears locked_by, sets status=available, version++.
	// The volume was never explicitly locked at admission for restore jobs,
	// so locked_by is NULL; UnlockVolume clears it unconditionally.
	if err := h.deps.Store.UnlockVolume(ctx, volID, db.VolumeStatusAvailable); err != nil {
		// Best-effort: mark volume as error so caller can inspect.
		_ = h.deps.Store.UnlockVolume(ctx, volID, db.VolumeStatusError)
		return fmt.Errorf("step4 unlock to available: %w", err)
	}
	log.Info("VOLUME_RESTORE: completed", "volume_id", volID, "storage_path", storagePath)
	return nil
}

// deriveRestoreStoragePath returns the control-plane storage path for a restored volume.
// Uses the LocalStorageManager when available for safe, configurable path derivation.
// Falls back to a deterministic legacy path when manager is nil.
func deriveRestoreStoragePath(mgr *runtime.LocalStorageManager, snapID, volID string) string {
	if mgr != nil {
		return mgr.RestoreVolumePath(snapID, volID)
	}
	return "/volumes/" + volID + "/restore-from-" + snapID + ".img"
}
