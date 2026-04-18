package handlers

// stop.go — INSTANCE_STOP job handler.
//
// Source: LIFECYCLE_STATE_MACHINE_V1 §2 (RUNNING→STOPPING→STOPPED),
//         04-02-lifecycle-action-flows.md §INSTANCE_STOP,
//         EVENTS_SCHEMA_V1 §instance.stopping / instance.stopped,
//         LIFECYCLE_STATE_MACHINE_V1 §6 (usage.end coupled to STOPPED write).
//
// Stop sequence:
//  1. DB: load instance; if already stopped or deleted → idempotent no-op.
//  2. Validate source state is running (or re-entrant stopping).
//  3. DB: transition running → stopping. Write stop.initiate event.
//  4. Host Agent: StopInstance (ACPI graceful, force on timeout).
//  5. Host Agent: DeleteInstance (releases VM process, TAP, rootfs).
//     Phase 1: stop always releases all runtime resources; root disk is gone.
//  6. IP retained (IP_ALLOCATION_CONTRACT_V1 §5) + NIC status → detached.
//  7. DB: transition stopping → stopped. Emit usage.end + instance.stopped events.
//
// Idempotency: if re-delivered while in stopping, resumes from step 4
// (host-agent ops are idempotent). Already stopped/deleted → immediate no-op.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// StopHandler handles INSTANCE_STOP jobs.
type StopHandler struct {
	deps           *Deps
	log            *slog.Logger
	runtimeFactory func(hostID, address string) RuntimeClient
}

// NewStopHandler constructs a StopHandler with production defaults.
func NewStopHandler(deps *Deps, log *slog.Logger) *StopHandler {
	h := &StopHandler{deps: deps, log: log}
	h.runtimeFactory = func(hostID, address string) RuntimeClient {
		return deps.Runtime(hostID, address)
	}
	return h
}

// Execute runs the full stop sequence. Idempotent on duplicate delivery.
func (h *StopHandler) Execute(ctx context.Context, job *db.JobRow) error {
	log := h.log.With("job_id", job.ID, "instance_id", job.InstanceID)
	log.Info("INSTANCE_STOP: starting")

	// ── Step 1: Load instance ─────────────────────────────────────────────────
	inst, err := h.deps.Store.GetInstanceByID(ctx, job.InstanceID)
	if err != nil {
		return fmt.Errorf("step1 load instance: %w", err)
	}

	// Idempotent: a prior delivery already completed successfully.
	if inst.VMState == "stopped" || inst.VMState == "deleted" {
		log.Info("INSTANCE_STOP: already terminal — idempotent no-op", "state", inst.VMState)
		return nil
	}

	// ── Step 2: Validate source state ────────────────────────────────────────
	// Valid entries: running (fresh) or stopping (re-entrant delivery).
	// Any other state is an illegal transition per LIFECYCLE_STATE_MACHINE_V1 §2.
	if inst.VMState != "running" && inst.VMState != "stopping" {
		return fmt.Errorf("INSTANCE_STOP: illegal source state %q — only running or stopping allowed", inst.VMState)
	}

	// ── Step 3: Transition running → stopping ────────────────────────────────
	// Skip if already stopping (re-entrant delivery).
	if inst.VMState == "running" {
		if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "running", "stopping", inst.Version); err != nil {
			return fmt.Errorf("step3 transition to stopping: %w", err)
		}
		inst.Version++
		inst.VMState = "stopping"
	}
	h.writeEvent(ctx, inst.ID, db.EventInstanceStopInitiate, "Stop initiated")
	log.Info("step3: instance stopping")

	// ── Steps 4 & 5: Runtime operations on Host Agent ─────────────────────────
	if inst.HostID != nil && *inst.HostID != "" {
		hostAddr := *inst.HostID + ":50051"
		rtClient := h.runtimeFactory(*inst.HostID, hostAddr)

		// Step 4: Stop VM process (ACPI graceful + force-kill on timeout).
		// Failure is retryable — do not mark failed here; let job retries exhaust.
		if _, err := rtClient.StopInstance(ctx, &runtimeclient.StopInstanceRequest{
			InstanceID:     inst.ID,
			TimeoutSeconds: 30,
		}); err != nil {
			return fmt.Errorf("step4 StopInstance: %w", err)
		}
		log.Info("step4: VM stopped on host")

		// Step 5: Release runtime resources (TAP + rootfs).
		// Phase 1 semantics: stop = full teardown; start re-provisions fresh.
		if _, err := rtClient.DeleteInstance(ctx, &runtimeclient.DeleteInstanceRequest{
			InstanceID:     inst.ID,
			DeleteRootDisk: true,
		}); err != nil {
			return fmt.Errorf("step5 DeleteInstance (resource teardown): %w", err)
		}
		log.Info("step5: VM resources released")
	}

	// ── Step 6: Retain IP + update NIC status to detached ───────────────────
	// IP_ALLOCATION_CONTRACT_V1 §5: private IP is stable across stop/start.
	// The ip_allocations row is NOT released here; the same IP will be reused
	// when the instance is started. Only the delete handler releases the IP.
	//
	// For VPC instances (primary NIC present), advance NIC status to "detached"
	// to reflect that the host-side TAP has been torn down. Phase 1 classic
	// instances (no NIC row) hit the nil guard and skip this block safely.
	nic, _ := h.deps.Store.GetPrimaryNetworkInterfaceByInstance(ctx, inst.ID)
	if nic != nil {
		if err := h.deps.Store.UpdateNetworkInterfaceStatus(ctx, nic.ID, "detached"); err != nil {
			log.Error("step6: UpdateNetworkInterfaceStatus(detached) failed — non-fatal", "nic_id", nic.ID, "error", err)
		} else {
			log.Info("step6: NIC detached", "nic_id", nic.ID)
		}
	}

	// ── Step 7: Transition stopping → stopped; emit usage.end ────────────────
	// usage.end is coupled to the STOPPED write per LIFECYCLE_STATE_MACHINE_V1 §6.
	if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "stopping", "stopped", inst.Version); err != nil {
		return fmt.Errorf("step7 transition to stopped: %w", err)
	}
	h.writeEvent(ctx, inst.ID, db.EventUsageEnd, "Usage billing stopped")
	h.writeEvent(ctx, inst.ID, db.EventInstanceStop, "Instance stopped")
	log.Info("INSTANCE_STOP: completed — instance stopped")
	return nil
}

func (h *StopHandler) writeEvent(ctx context.Context, instanceID, eventType, msg string) {
	_ = h.deps.Store.InsertEvent(ctx, &db.EventRow{
		ID:         idgen.New(idgen.PrefixEvent),
		InstanceID: instanceID,
		EventType:  eventType,
		Message:    msg,
		Actor:      "system",
	})
}

// SetRuntimeFactory overrides the runtime client factory. Used by integration tests.
func (h *StopHandler) SetRuntimeFactory(f func(hostID, address string) RuntimeClient) {
	h.runtimeFactory = f
}
