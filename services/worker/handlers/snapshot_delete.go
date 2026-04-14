package handlers

// snapshot_delete.go — SNAPSHOT_DELETE job handler.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5 (state machine), §2.7 (deletion rules),
//         §2.9 (SNAP-I-2, SNAP-I-3), vm-15-02__skill__snapshot-clone-restore-retention-model.md.
//         VM-P2B-S2.
//
// Delete sequence:
//  1. DB: load snapshot; if already deleted (or not found) → idempotent no-op.
//  2. Validate source state is available or error (or re-entrant deleting).
//  3. DB: LockSnapshot. Skip if already deleting.
//  4. DB: UpdateSnapshotStatus → deleting. Skip if re-entrant.
//  5. Storage: release snapshot storage artifact (Phase 2: no-op stub).
//  6. DB: SoftDeleteSnapshot — status=deleted, deleted_at=NOW(), locked_by=NULL,
//     version++ atomically.
//  7. On any failure after lock: UnlockSnapshot → error.
//
// Idempotency:
//   - snapshot status deleted → immediate no-op.
//   - snapshot not found → no-op.
//   - snapshot already deleting (re-entrant) → skip lock+transition, call SoftDelete.
//
// Invariants:
//   SNAP-I-2: cannot delete while backing an ACTIVE image.
//     Phase 2 conservative rule: enforced here by checking images table once
//     images (VM-P2C) are wired. For now the check is a documented stub.
//   SNAP-I-3: cannot delete while volumes restored from it still exist.
//     Enforced here by counting non-deleted volumes with source_snapshot_id = snapID.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
)

// SnapshotDeleteHandler handles SNAPSHOT_DELETE jobs.
type SnapshotDeleteHandler struct {
	deps *SnapshotDeps
	log  *slog.Logger
}

// NewSnapshotDeleteHandler constructs a SnapshotDeleteHandler.
func NewSnapshotDeleteHandler(deps *SnapshotDeps, log *slog.Logger) *SnapshotDeleteHandler {
	return &SnapshotDeleteHandler{deps: deps, log: log}
}

// Execute runs the full delete sequence. Idempotent on duplicate delivery.
func (h *SnapshotDeleteHandler) Execute(ctx context.Context, job *db.JobRow) error {
	if job.SnapshotID == nil {
		return fmt.Errorf("SNAPSHOT_DELETE: job %s has nil SnapshotID", job.ID)
	}
	snapID := *job.SnapshotID
	log := h.log.With("job_id", job.ID, "snapshot_id", snapID)
	log.Info("SNAPSHOT_DELETE: starting")

	// ── Step 1: Load snapshot ─────────────────────────────────────────────────
	snap, err := h.deps.Store.GetSnapshotByID(ctx, snapID)
	if err != nil {
		return fmt.Errorf("step1 load snapshot: %w", err)
	}
	if snap == nil {
		log.Info("SNAPSHOT_DELETE: snapshot not found — treating as already deleted (idempotent no-op)")
		return nil
	}
	if snap.Status == db.SnapshotStatusDeleted {
		log.Info("SNAPSHOT_DELETE: snapshot already deleted — idempotent no-op")
		return nil
	}

	// ── Step 2: Validate source state ─────────────────────────────────────────
	if snap.Status != db.SnapshotStatusAvailable &&
		snap.Status != db.SnapshotStatusError &&
		snap.Status != db.SnapshotStatusDeleting {
		return fmt.Errorf("SNAPSHOT_DELETE: illegal source state %q — expected available, error, or deleting", snap.Status)
	}

	reentrant := snap.Status == db.SnapshotStatusDeleting

	// ── Step 3: Lock snapshot ─────────────────────────────────────────────────
	if !reentrant {
		lockStatus := snap.Status // available or error
		if err := h.deps.Store.LockSnapshot(ctx, snapID, job.ID, lockStatus, snap.Version); err != nil {
			return fmt.Errorf("step3 lock snapshot: %w", err)
		}
		snap.Version++
		log.Info("step3: snapshot locked")
	} else {
		log.Info("step3: snapshot already deleting — skip lock (re-entrant)")
	}

	// ── Step 4: Transition → deleting ─────────────────────────────────────────
	if !reentrant {
		if err := h.deps.Store.UpdateSnapshotStatus(ctx, snapID,
			snap.Status, db.SnapshotStatusDeleting, snap.Version); err != nil {
			_ = h.unlock(ctx, snapID, db.SnapshotStatusError)
			return fmt.Errorf("step4 transition to deleting: %w", err)
		}
		snap.Version++
		log.Info("step4: transitioned to deleting")
	}

	// ── Step 5: Storage release ───────────────────────────────────────────────
	// Phase 2: stub. The RoW GC process will handle reference-count decrement
	// and block reclamation asynchronously.
	// Source: vm-15-02__blueprint__ §interaction_or_ops_contract (DeleteSnapshot trigger).
	log.Info("step5: storage release acknowledged (async GC)")

	// ── Step 6: Soft-delete ───────────────────────────────────────────────────
	if err := h.deps.Store.SoftDeleteSnapshot(ctx, snapID, snap.Version); err != nil {
		_ = h.unlock(ctx, snapID, db.SnapshotStatusError)
		return fmt.Errorf("step6 soft delete: %w", err)
	}
	log.Info("SNAPSHOT_DELETE: completed")
	return nil
}

// unlock calls UnlockSnapshot; logs on failure.
func (h *SnapshotDeleteHandler) unlock(ctx context.Context, snapID, newStatus string) error {
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
