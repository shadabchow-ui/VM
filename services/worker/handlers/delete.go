package handlers

// delete.go — INSTANCE_DELETE job handler.
//
// Source: IMPLEMENTATION_PLAN_V1 §39, 04-02-lifecycle-action-flows.md §INSTANCE_DELETE.
//
// Delete sequence (reverse-allocation order — R-06):
//  1. DB: load instance; if already deleted → idempotent no-op.
//  2. DB: transition → deleting.
//  3. DB: look up root disk to determine delete_on_termination.
//         Source: 06-01-root-disk-model-and-persistence-semantics.md §Delete Semantics.
//  4. Host Agent: StopInstance (if running).
//  5. Host Agent: DeleteInstance. Pass DeleteRootDisk derived from delete_on_termination.
//  6. DB: apply root disk disposition:
//         delete_on_termination=true  → DeleteRootDisk (row removed).
//         delete_on_termination=false → DetachRootDisk (instance_id=NULL, status=DETACHED).
//         No disk record (pre-Slice-3 instances) → safe no-op.
//  7. Network: ReleaseIP (idempotent).
//  8. DB: SoftDeleteInstance (vm_state=deleted, deleted_at=NOW()). Write usage.end.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// DeleteHandler handles INSTANCE_DELETE jobs.
type DeleteHandler struct {
	deps *Deps
	log  *slog.Logger
	// runtimeFactory is overridable for tests.
	runtimeFactory func(hostID, address string) RuntimeClient
}

// NewDeleteHandler constructs a DeleteHandler with production defaults.
func NewDeleteHandler(deps *Deps, log *slog.Logger) *DeleteHandler {
	h := &DeleteHandler{deps: deps, log: log}
	h.runtimeFactory = func(hostID, address string) RuntimeClient {
		return deps.Runtime(hostID, address)
	}
	return h
}

// Execute runs the full delete sequence. Idempotent on duplicate delivery.
func (h *DeleteHandler) Execute(ctx context.Context, job *db.JobRow) error {
	log := h.log.With("job_id", job.ID, "instance_id", job.InstanceID)
	log.Info("INSTANCE_DELETE: starting")

	// ── Step 1: Load instance ─────────────────────────────────────────────────
	inst, err := h.deps.Store.GetInstanceByID(ctx, job.InstanceID)
	if err != nil {
		return fmt.Errorf("step1 load instance: %w", err)
	}
	if inst.VMState == "deleted" {
		log.Info("instance already deleted — idempotent no-op")
		return nil
	}

	// ── Step 2: transition → deleting ────────────────────────────────────────
	prevState := inst.VMState
	if inst.VMState != "deleting" {
		if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, inst.VMState, "deleting", inst.Version); err != nil {
			return fmt.Errorf("step2 transition to deleting: %w", err)
		}
		inst.Version++
		inst.VMState = "deleting"
	}
	h.writeEvent(ctx, inst.ID, db.EventInstanceDeleteInitiate, "Delete initiated")
	log.Info("step2: deleting", "prev_state", prevState)

	// ── Step 3: Determine root disk disposition ───────────────────────────────
	// Look up the root disk to honor delete_on_termination.
	// Source: 06-01-root-disk-model-and-persistence-semantics.md §Delete Semantics.
	// If no disk record exists (instance created before Slice 3 wiring), treat
	// as delete_on_termination=true for backward compatibility.
	disk, err := h.deps.Store.GetRootDiskByInstanceID(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("step3 get root disk: %w", err)
	}
	deleteRootDisk := true // default: Phase 1 behavior
	if disk != nil {
		deleteRootDisk = disk.DeleteOnTermination
	}
	log.Info("step3: root disk disposition determined",
		"disk_found", disk != nil,
		"delete_on_termination", deleteRootDisk,
	)

	// ── Steps 4 & 5: Stop + Delete on host agent ──────────────────────────────
	if inst.HostID != nil && *inst.HostID != "" {
		hostAddr := *inst.HostID + ":50051"
		rtClient := h.runtimeFactory(*inst.HostID, hostAddr)

		if prevState == "running" || prevState == "stopping" || prevState == "starting" {
			if _, err := rtClient.StopInstance(ctx, &runtimeclient.StopInstanceRequest{
				InstanceID: inst.ID, TimeoutSeconds: 30,
			}); err != nil {
				log.Warn("step4: StopInstance failed — continuing", "error", err)
			} else {
				log.Info("step4: VM stopped")
			}
		}

		// Pass delete_on_termination to host agent so it knows whether to
		// physically remove the qcow2 file from NFS storage.
		// Source: 06-01-root-disk-model-and-persistence-semantics.md §CoW Implementation.
		if _, err := rtClient.DeleteInstance(ctx, &runtimeclient.DeleteInstanceRequest{
			InstanceID:     inst.ID,
			DeleteRootDisk: deleteRootDisk,
		}); err != nil {
			return fmt.Errorf("step5 DeleteInstance: %w", err)
		}
		log.Info("step5: VM resources deleted", "delete_root_disk", deleteRootDisk)
	}

	// ── Step 6: Apply root disk DB disposition ────────────────────────────────
	// Source: 06-01-root-disk-model-and-persistence-semantics.md §Delete Semantics.
	if disk != nil {
		if disk.DeleteOnTermination {
			// delete_on_termination=true: remove the root_disks row.
			// Physical file was deleted by host agent above.
			if err := h.deps.Store.DeleteRootDisk(ctx, disk.DiskID); err != nil {
				// Non-fatal: log and continue. Orphan GC will clean up.
				log.Error("step6: DeleteRootDisk failed — non-fatal", "disk_id", disk.DiskID, "error", err)
			} else {
				log.Info("step6: root disk record deleted", "disk_id", disk.DiskID)
			}
		} else {
			// delete_on_termination=false: detach disk (instance_id=NULL, status=DETACHED).
			// This is the Phase 2 persistent volume entry point.
			// Source: P2_VOLUME_MODEL.md §1.
			if err := h.deps.Store.DetachRootDisk(ctx, disk.DiskID); err != nil {
				log.Error("step6: DetachRootDisk failed — non-fatal", "disk_id", disk.DiskID, "error", err)
			} else {
				log.Info("step6: root disk detached (retained for Phase 2)", "disk_id", disk.DiskID)
			}
		}
	}

	// ── Step 7: Release IP ────────────────────────────────────────────────────
	ip, _ := h.deps.Store.GetIPByInstance(ctx, inst.ID)
	if ip != "" {
		if err := h.deps.Network.ReleaseIP(ctx, ip, phase1VPCID, inst.ID); err != nil {
			log.Error("step7: ReleaseIP failed — IP may be leaked", "ip", ip, "error", err)
		} else {
			log.Info("step7: IP released", "ip", ip)
			h.writeEvent(ctx, inst.ID, db.EventIPReleased, "IP released: "+ip)
		}
	}

	// ── Step 8: Soft-delete ───────────────────────────────────────────────────
	if err := h.deps.Store.SoftDeleteInstance(ctx, inst.ID, inst.Version); err != nil {
		return fmt.Errorf("step8 soft delete: %w", err)
	}
	h.writeEvent(ctx, inst.ID, db.EventUsageEnd, "Usage billing stopped")
	h.writeEvent(ctx, inst.ID, db.EventInstanceDelete, "Instance deleted")
	log.Info("INSTANCE_DELETE: completed")
	return nil
}

func (h *DeleteHandler) writeEvent(ctx context.Context, instanceID, eventType, msg string) {
	_ = h.deps.Store.InsertEvent(ctx, &db.EventRow{
		ID:         idgen.New(idgen.PrefixEvent),
		InstanceID: instanceID,
		EventType:  eventType,
		Message:    msg,
		Actor:      "system",
	})
}

// SetRuntimeFactory overrides the runtime client factory. Used by integration tests.
func (h *DeleteHandler) SetRuntimeFactory(f func(hostID, address string) RuntimeClient) {
	h.runtimeFactory = f
}
