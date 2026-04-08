package handlers

// reboot.go — INSTANCE_REBOOT job handler.
//
// Source: LIFECYCLE_STATE_MACHINE_V1 §2 (RUNNING→REBOOTING→RUNNING),
//         04-02-lifecycle-action-flows.md §INSTANCE_REBOOT,
//         EVENTS_SCHEMA_V1 §instance.rebooting / instance.reboot.complete.
//
// Reboot sequence:
//  1. DB: load instance; validate source state.
//  2. Validate source state is running (fresh entry) or rebooting (re-entrant).
//  3. DB: transition running → rebooting. Write reboot.initiate event.
//  4. Host Agent: StopInstance (graceful stop on the same host).
//  5. Host Agent: CreateInstance (re-launch on the same host with same IP).
//     Reboot retains: same host, same IP, same rootfs path.
//     The host agent's DeleteInstance is NOT called — rootfs is preserved.
//  6. Readiness: wait for SSH port (up to 120s). Mockable via readinessFn.
//  7. DB: transition rebooting → running. Emit reboot.complete event.
//
// On failure: transition to failed (not back to running).
//
// Idempotency: if re-delivered while in rebooting, resumes from step 4
// (host-agent ops are idempotent on the host agent side).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// RebootHandler handles INSTANCE_REBOOT jobs.
type RebootHandler struct {
	deps           *Deps
	log            *slog.Logger
	runtimeFactory func(hostID, address string) RuntimeClient
	readinessFn    func(ctx context.Context, ip string, timeout time.Duration) error
}

// NewRebootHandler constructs a RebootHandler with production defaults.
func NewRebootHandler(deps *Deps, log *slog.Logger) *RebootHandler {
	h := &RebootHandler{deps: deps, log: log}
	h.runtimeFactory = func(hostID, address string) RuntimeClient {
		return deps.Runtime(hostID, address)
	}
	h.readinessFn = waitForSSH
	return h
}

// Execute runs the full reboot sequence. Idempotent on duplicate delivery.
func (h *RebootHandler) Execute(ctx context.Context, job *db.JobRow) error {
	log := h.log.With("job_id", job.ID, "instance_id", job.InstanceID)
	log.Info("INSTANCE_REBOOT: starting")

	// ── Step 1: Load instance ─────────────────────────────────────────────────
	inst, err := h.deps.Store.GetInstanceByID(ctx, job.InstanceID)
	if err != nil {
		return fmt.Errorf("step1 load instance: %w", err)
	}

	// ── Step 2: Validate source state ────────────────────────────────────────
	// Valid entries: running (fresh) or rebooting (re-entrant delivery).
	if inst.VMState != "running" && inst.VMState != "rebooting" {
		return fmt.Errorf("INSTANCE_REBOOT: illegal source state %q — only running or rebooting allowed", inst.VMState)
	}

	// Host must be assigned — reboot is on the same host.
	if inst.HostID == nil || *inst.HostID == "" {
		return fmt.Errorf("INSTANCE_REBOOT: instance has no assigned host")
	}

	// ── Step 3: Transition running → rebooting ────────────────────────────────
	// Skip if already rebooting (re-entrant delivery).
	if inst.VMState == "running" {
		if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "running", "rebooting", inst.Version); err != nil {
			return fmt.Errorf("step3 transition to rebooting: %w", err)
		}
		inst.Version++
		inst.VMState = "rebooting"
	}
	h.writeEvent(ctx, inst.ID, db.EventInstanceRebootInitiate, "Reboot initiated")
	log.Info("step3: instance rebooting")

	// Retrieve the current IP — it is retained across reboot.
	allocatedIP, _ := h.deps.Store.GetIPByInstance(ctx, inst.ID)

	hostAddr := *inst.HostID + ":50051"
	rtClient := h.runtimeFactory(*inst.HostID, hostAddr)

	// ── Step 4: Stop VM process on host agent ─────────────────────────────────
	// Does NOT call DeleteInstance — rootfs and TAP are preserved for reboot.
	if _, err := rtClient.StopInstance(ctx, &runtimeclient.StopInstanceRequest{
		InstanceID:     inst.ID,
		TimeoutSeconds: 30,
	}); err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step4 StopInstance: %w", err))
	}
	log.Info("step4: VM stopped for reboot")

	// ── Step 5: Re-launch VM on the same host ─────────────────────────────────
	// CreateInstance on host agent is idempotent — if the VM process already
	// exists it returns success. Same rootfs path, same TAP, same IP.
	createReq := &runtimeclient.CreateInstanceRequest{
		InstanceID:     inst.ID,
		ImageURL:       imageURLFromID(inst.ImageID),
		InstanceTypeID: inst.InstanceTypeID,
		CPUCores:       shapeCPU(inst.InstanceTypeID),
		MemoryMB:       shapeMemMB(inst.InstanceTypeID),
		DiskGB:         shapeDiskGB(inst.InstanceTypeID),
		RootfsPath:     fmt.Sprintf("/mnt/nfs/vols/%s.qcow2", inst.ID),
		Network: runtimeclient.NetworkConfig{
			PrivateIP:  allocatedIP,
			TapDevice:  "tap-" + inst.ID[:8],
			MacAddress: deriveMACAddress(inst.ID),
		},
	}
	if _, err := rtClient.CreateInstance(ctx, createReq); err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step5 CreateInstance (reboot): %w", err))
	}
	log.Info("step5: VM re-launched on host")

	// ── Step 6: Wait for readiness ────────────────────────────────────────────
	if os.Getenv("READINESS_DRY_RUN") == "true" {
		log.Warn("READINESS_DRY_RUN=true: skipping SSH readiness check")
	} else {
		if err := h.readinessFn(ctx, allocatedIP, readinessTimeout); err != nil {
			return h.failInstance(ctx, inst, fmt.Errorf("step6 readiness timeout: %w", err))
		}
	}
	log.Info("step6: VM ready after reboot")

	// ── Step 7: Transition rebooting → running ────────────────────────────────
	if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "rebooting", "running", inst.Version); err != nil {
		return fmt.Errorf("step7 transition to running: %w", err)
	}
	h.writeEvent(ctx, inst.ID, db.EventInstanceReboot, "Reboot completed. Instance is running.")
	log.Info("INSTANCE_REBOOT: completed — instance running")
	return nil
}

// failInstance transitions to failed, writes an event, returns the cause.
func (h *RebootHandler) failInstance(ctx context.Context, inst *db.InstanceRow, cause error) error {
	if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, inst.VMState, "failed", inst.Version); err != nil {
		h.log.Error("failInstance: could not set failed", "instance_id", inst.ID, "error", err)
	}
	inst.VMState = "failed"
	h.writeEvent(ctx, inst.ID, db.EventInstanceFailure, cause.Error())
	return cause
}

func (h *RebootHandler) writeEvent(ctx context.Context, instanceID, eventType, msg string) {
	_ = h.deps.Store.InsertEvent(ctx, &db.EventRow{
		ID:         idgen.New(idgen.PrefixEvent),
		InstanceID: instanceID,
		EventType:  eventType,
		Message:    msg,
		Actor:      "system",
	})
}

// SetRuntimeFactory overrides the runtime client factory. Used by integration tests.
func (h *RebootHandler) SetRuntimeFactory(f func(hostID, address string) RuntimeClient) {
	h.runtimeFactory = f
}

// SetReadinessFn overrides the readiness check function. Used by integration tests.
func (h *RebootHandler) SetReadinessFn(f func(ctx context.Context, ip string, timeout time.Duration) error) {
	h.readinessFn = f
}
