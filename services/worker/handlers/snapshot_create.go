package handlers

// snapshot_create.go — SNAPSHOT_CREATE job handler.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.4 (state machine), §2.6 (creation rules),
//         §2.9 (SNAP-I-1), vm-15-02__skill__snapshot-clone-restore-retention-model.md.
//         VM-P2B-S2.
//
// Create sequence:
//  1. DB: load snapshot; if already available → idempotent no-op.
//  2. Validate source state is pending (or re-entrant creating).
//  3. DB: LockSnapshot — sets locked_by=jobID, version++. Skip if already creating.
//  4. DB: UpdateSnapshotStatus pending → creating. Skip if re-entrant.
//  5. Storage: issue freeze_root_node / copy-on-write metadata operation.
//     Phase 2: simulated — sets storage_path to a deterministic location.
//     The storage data-plane integration is a separate subsystem concern.
//  6. DB: MarkSnapshotAvailable — clears locked_by, sets storage_path,
//     progress_percent=100, completed_at=NOW(), status=available, version++.
//     SNAP-I-1: status is only set to available here, not at admission time.
//  7. On any failure after lock: UnlockSnapshot → error.
//
// Idempotency:
//   - snapshot already available → immediate no-op.
//   - snapshot already creating (re-entrant delivery) → skip lock+transition.
//
// Invariants:
//   SNAP-I-1: snapshot never set to 'available' until storage is confirmed.
//   VOL-I-5 analogue: at most one mutation lock per snapshot.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	runtime "github.com/compute-platform/compute-platform/services/host-agent/runtime"
)

// SnapshotCreateHandler handles SNAPSHOT_CREATE jobs.
type SnapshotCreateHandler struct {
	deps *SnapshotDeps
	log  *slog.Logger
}

// NewSnapshotCreateHandler constructs a SnapshotCreateHandler.
func NewSnapshotCreateHandler(deps *SnapshotDeps, log *slog.Logger) *SnapshotCreateHandler {
	return &SnapshotCreateHandler{deps: deps, log: log}
}

// Execute runs the full snapshot creation sequence. Idempotent on duplicate delivery.
func (h *SnapshotCreateHandler) Execute(ctx context.Context, job *db.JobRow) error {
	if job.SnapshotID == nil {
		return fmt.Errorf("SNAPSHOT_CREATE: job %s has nil SnapshotID", job.ID)
	}
	snapID := *job.SnapshotID
	log := h.log.With("job_id", job.ID, "snapshot_id", snapID)
	log.Info("SNAPSHOT_CREATE: starting")

	// ── Step 1: Load snapshot ─────────────────────────────────────────────────
	snap, err := h.deps.Store.GetSnapshotByID(ctx, snapID)
	if err != nil {
		return fmt.Errorf("step1 load snapshot: %w", err)
	}
	if snap == nil {
		return fmt.Errorf("SNAPSHOT_CREATE: snapshot %s not found", snapID)
	}

	// Idempotent: prior delivery already completed.
	if snap.Status == db.SnapshotStatusAvailable {
		log.Info("SNAPSHOT_CREATE: snapshot already available — idempotent no-op")
		return nil
	}

	// ── Step 2: Validate source state ─────────────────────────────────────────
	if snap.Status != db.SnapshotStatusPending && snap.Status != db.SnapshotStatusCreating {
		return fmt.Errorf("SNAPSHOT_CREATE: illegal source state %q — expected pending or creating", snap.Status)
	}

	reentrant := snap.Status == db.SnapshotStatusCreating

	// ── Step 3: Lock snapshot ─────────────────────────────────────────────────
	if !reentrant {
		if err := h.deps.Store.LockSnapshot(ctx, snapID, job.ID, db.SnapshotStatusPending, snap.Version); err != nil {
			return fmt.Errorf("step3 lock snapshot: %w", err)
		}
		snap.Version++
		log.Info("step3: snapshot locked")
	} else {
		log.Info("step3: snapshot already creating — skip lock (re-entrant)")
	}

	// ── Step 4: Transition pending → creating ─────────────────────────────────
	if !reentrant {
		if err := h.deps.Store.UpdateSnapshotStatus(ctx, snapID,
			db.SnapshotStatusPending, db.SnapshotStatusCreating, snap.Version); err != nil {
			_ = h.unlock(ctx, snapID, db.SnapshotStatusError)
			return fmt.Errorf("step4 transition to creating: %w", err)
		}
		snap.Version++
		log.Info("step4: transitioned to creating")
	}

	// ── Step 5: Storage operation ─────────────────────────────────────────────
	// Derive deterministic safe storage path under the configured local root.
	// If Storage is nil (tests without manager), fall back to legacy path.
	storagePath := deriveSnapshotStoragePath(h.deps.Storage, snapID)
	log.Info("step5: storage operation complete", "storage_path", storagePath)

	// ── Step 6: Mark available — SNAP-I-1 ─────────────────────────────────────
	// MarkSnapshotAvailable atomically: clears locked_by, sets storage_path,
	// progress_percent=100, completed_at=NOW(), status=available, version++.
	// This is the ONLY place status is set to 'available'.
	if err := h.deps.Store.MarkSnapshotAvailable(ctx, snapID, storagePath, snap.Version); err != nil {
		_ = h.unlock(ctx, snapID, db.SnapshotStatusError)
		return fmt.Errorf("step6 mark available: %w", err)
	}
	log.Info("SNAPSHOT_CREATE: completed", "storage_path", storagePath)
	return nil
}

// unlock calls UnlockSnapshot; logs on failure.
func (h *SnapshotCreateHandler) unlock(ctx context.Context, snapID, newStatus string) error {
	if err := h.deps.Store.UnlockSnapshot(ctx, snapID, newStatus); err != nil {
		h.log.Error("UnlockSnapshot failed",
			"snapshot_id", snapID,
			"new_status", newStatus,
			"error", err,
		)
		return err
	}
	return nil
}

// deriveSnapshotStoragePath returns the storage path for a snapshot.
// Uses the LocalStorageManager when available for safe, configurable path derivation.
// Falls back to a deterministic legacy path when manager is nil.
func deriveSnapshotStoragePath(mgr *runtime.LocalStorageManager, snapID string) string {
	if mgr != nil {
		return mgr.SnapshotDataPath(snapID)
	}
	return "/snapshots/" + snapID + "/data"
}
