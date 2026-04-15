package handlers

// snapshot_delete.go — SNAPSHOT_DELETE job handler.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5 (state machine), §2.7 (deletion rules),
//         §2.9 (SNAP-I-2, SNAP-I-3), vm-15-02__skill__snapshot-clone-restore-retention-model.md.
//         VM-P2B-S2.
//         VM-P2B-S3: enforce SNAP-I-3 at the worker level via CountVolumesBySourceSnapshot.
//
// Delete sequence:
//  1. DB: load snapshot; if already deleted (or not found) → idempotent no-op.
//  2. Validate source state is available or error (or re-entrant deleting).
//  3. SNAP-I-3: CountVolumesBySourceSnapshot — fail if any non-deleted volumes
//     were restored from this snapshot. Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.
//  4. DB: LockSnapshot. Skip if already deleting.
//  5. DB: UpdateSnapshotStatus → deleting. Skip if re-entrant.
//  6. Storage: release snapshot storage artifact (Phase 2: no-op stub).
//  7. DB: SoftDeleteSnapshot — status=deleted, deleted_at=NOW(), locked_by=NULL,
//     version++ atomically.
//  8. On any failure after lock: UnlockSnapshot → error.
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
//     Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.

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

	// ── Step 3: SNAP-I-3 — reject if restored volumes still exist ────────────
	// Count non-deleted volumes whose source_snapshot_id = snapID.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.
	// Skip on re-entrant: if we already transitioned to deleting in a prior
	// run, this check passed then and we should not block completion.
	if !reentrant {
		volCount, err := h.deps.Store.CountVolumesBySourceSnapshot(ctx, snapID)
		if err != nil {
			return fmt.Errorf("step3 count restored volumes: %w", err)
		}
		if volCount > 0 {
			return fmt.Errorf("SNAPSHOT_DELETE: SNAP-I-3: snapshot %s has %d non-deleted volume(s) restored from it — delete those volumes first", snapID, volCount)
		}
		log.Info("step3: SNAP-I-3 check passed — no active restored volumes")
	}

	// ── Step 4: Lock snapshot ─────────────────────────────────────────────────
	if !reentrant {
		lockStatus := snap.Status // available or error
		if err := h.deps.Store.LockSnapshot(ctx, snapID, job.ID, lockStatus, snap.Version); err != nil {
			return fmt.Errorf("step4 lock snapshot: %w", err)
		}
		snap.Version++
		log.Info("step4: snapshot locked")
	} else {
		log.Info("step4: snapshot already deleting — skip lock (re-entrant)")
	}

	// ── Step 5: Transition → deleting ─────────────────────────────────────────
	if !reentrant {
		if err := h.deps.Store.UpdateSnapshotStatus(ctx, snapID,
			snap.Status, db.SnapshotStatusDeleting, snap.Version); err != nil {
			_ = h.unlock(ctx, snapID, db.SnapshotStatusError)
			return fmt.Errorf("step5 transition to deleting: %w", err)
		}
		snap.Version++
		log.Info("step5: transitioned to deleting")
	}

	// ── Step 6: Storage release ───────────────────────────────────────────────
	// Phase 2: stub. The RoW GC process will handle reference-count decrement
	// and block reclamation asynchronously.
	// Source: vm-15-02__blueprint__ §interaction_or_ops_contract (DeleteSnapshot trigger).
	log.Info("step6: storage release acknowledged (async GC)")

	// ── Step 7: Soft-delete ───────────────────────────────────────────────────
	if err := h.deps.Store.SoftDeleteSnapshot(ctx, snapID, snap.Version); err != nil {
		_ = h.unlock(ctx, snapID, db.SnapshotStatusError)
		return fmt.Errorf("step7 soft delete: %w", err)
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
