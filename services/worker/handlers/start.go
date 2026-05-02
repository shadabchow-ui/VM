package handlers

// start.go — INSTANCE_START job handler.
//
// Source: LIFECYCLE_STATE_MACHINE_V1 §2 (STOPPED→PROVISIONING→RUNNING),
//         04-02-lifecycle-action-flows.md §INSTANCE_START,
//         04-01-create-instance-flow.md (provisioning sequence re-used),
//         EVENTS_SCHEMA_V1 §instance.start.initiate / usage.start.
//
// Start sequence (mirrors create.go provisioning sequence — R-06):
//  1. DB: load instance; validate source state is stopped (or re-entrant provisioning).
//  2. DB: transition stopped → provisioning. Write start.initiate event.
//  3. Scheduler: select host (first-fit by free CPU).
//  4. DB: assign host_id (re-assignment — prior host resources were released on stop).
//  5. Network: allocate IP (SELECT FOR UPDATE SKIP LOCKED).
//  6. Host Agent: CreateInstance (rootfs + TAP + NAT + Firecracker).
//  7. Readiness: wait for SSH port (up to 120s). Mockable via readinessFn for tests.
//  8. DB: transition provisioning → running. Emit usage.start + provisioning.done events.
//
// Phase 1 semantics: stop released all runtime resources (TAP, rootfs, IP).
// Start is therefore a full re-provision, not a resume.
// The CreateInstance path is re-used directly (identical to INSTANCE_CREATE).
//
// Idempotency: if re-delivered while in provisioning, resumes from step 3
// (host selection and all downstream steps are idempotent).

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

// StartHandler handles INSTANCE_START jobs.
type StartHandler struct {
	deps           *Deps
	log            *slog.Logger
	runtimeFactory func(hostID, address string) RuntimeClient
	readinessFn    func(ctx context.Context, ip string, timeout time.Duration) error
}

// NewStartHandler constructs a StartHandler with production defaults.
func NewStartHandler(deps *Deps, log *slog.Logger) *StartHandler {
	h := &StartHandler{deps: deps, log: log}
	h.runtimeFactory = func(hostID, address string) RuntimeClient {
		return deps.Runtime(hostID, address)
	}
	h.readinessFn = waitForSSH
	return h
}

// Execute runs the full start (re-provision) sequence. Idempotent on duplicate delivery.
func (h *StartHandler) Execute(ctx context.Context, job *db.JobRow) error {
	log := h.log.With("job_id", job.ID, "instance_id", job.InstanceID)
	log.Info("INSTANCE_START: starting")

	// ── Step 1: Load instance & validate state ───────────────────────────────
	inst, err := h.deps.Store.GetInstanceByID(ctx, job.InstanceID)
	if err != nil {
		return fmt.Errorf("step1 load instance: %w", err)
	}

	// Idempotent: if somehow running already, the prior delivery completed.
	if inst.VMState == "running" {
		log.Info("INSTANCE_START: already running — idempotent no-op")
		return nil
	}

	// Valid entries: stopped (fresh) or provisioning (re-entrant delivery).
	if inst.VMState != "stopped" && inst.VMState != "provisioning" {
		return fmt.Errorf("INSTANCE_START: illegal source state %q — only stopped or provisioning allowed", inst.VMState)
	}

	// ── Step 2: Transition stopped → provisioning ────────────────────────────
	// Skip if already provisioning (re-entrant delivery from a prior attempt).
	if inst.VMState == "stopped" {
		if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "stopped", "provisioning", inst.Version); err != nil {
			return fmt.Errorf("step2 transition to provisioning: %w", err)
		}
		inst.Version++
		inst.VMState = "provisioning"
	}
	h.writeEvent(ctx, inst.ID, db.EventInstanceStartInitiate, "Start initiated")
	h.writeEvent(ctx, inst.ID, db.EventInstanceProvisioningStart, "Re-provisioning started")
	log.Info("step2: instance provisioning")

	// ── Step 3: Select host ───────────────────────────────────────────────────
	hosts, err := h.deps.Store.GetAvailableHosts(ctx)
	if err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step3 get hosts: %w", err))
	}
	if len(hosts) == 0 {
		return h.failInstance(ctx, inst, fmt.Errorf("step3: no available hosts"))
	}
	selectedHost := hosts[0] // first-fit; hosts sorted by free CPU desc
	log.Info("step3: host selected", "host_id", selectedHost.ID)

	// ── Step 4: Assign host ───────────────────────────────────────────────────
	// Re-assigns host_id; the prior host may be different or gone.
	if err := h.deps.Store.AssignHost(ctx, inst.ID, selectedHost.ID, inst.Version); err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step4 assign host: %w", err))
	}
	inst.Version++
	inst.HostID = &selectedHost.ID
	log.Info("step4: host assigned", "host_id", selectedHost.ID)

	// ── Step 5: Retrieve retained IP ───────────────────────────────────────────
	// IP_ALLOCATION_CONTRACT_V1 §5: the private IP is retained across stop/start.
	// The stop handler does NOT release the IP, so GetIPByInstance returns the
	// same allocation that was made during INSTANCE_CREATE. No new AllocateIP call
	// is issued here; the ip_allocations row remains owned by this instance.
	allocatedIP, err := h.deps.Store.GetIPByInstance(ctx, inst.ID)
	if err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step5 GetIPByInstance: %w", err))
	}
	if allocatedIP == "" {
		// No retained IP found — fall back to fresh allocation.
		// This handles instances created before IP retention was enforced,
		// or any edge-case where the allocation was lost.
		var allocErr error
		allocatedIP, allocErr = h.deps.Network.AllocateIP(ctx, phase1VPCID, inst.ID)
		if allocErr != nil {
			return h.failInstance(ctx, inst, fmt.Errorf("step5 AllocateIP (fallback): %w", allocErr))
		}
		h.writeEvent(ctx, inst.ID, db.EventIPAllocated, "IP allocated (fallback): "+allocatedIP)
	}
	log.Info("step5: IP resolved", "ip", allocatedIP, "retained", true)

	// ── Step 6: CreateInstance on Host Agent ──────────────────────────────────
	hostAddr := selectedHost.ID + ":50051"
	rtClient := h.runtimeFactory(selectedHost.ID, hostAddr)

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
		// Rollback: release IP, then fail instance.
		if relErr := h.deps.Network.ReleaseIP(ctx, allocatedIP, phase1VPCID, inst.ID); relErr != nil {
			log.Error("rollback: IP release failed", "error", relErr)
		}
		return h.failInstance(ctx, inst, fmt.Errorf("step6 CreateInstance: %w", err))
	}
	log.Info("step6: VM running on host")

	// ── Step 7: Wait for readiness ────────────────────────────────────────────
	if os.Getenv("READINESS_DRY_RUN") == "true" {
		log.Warn("READINESS_DRY_RUN=true: skipping SSH readiness check")
	} else {
		if err := h.readinessFn(ctx, allocatedIP, readinessTimeout); err != nil {
			// Rollback: delete VM resources + release IP.
			if _, delErr := rtClient.DeleteInstance(ctx, &runtimeclient.DeleteInstanceRequest{
				InstanceID: inst.ID, DeleteRootDisk: true,
			}); delErr != nil {
				log.Error("rollback: DeleteInstance failed", "error", delErr)
			}
			if relErr := h.deps.Network.ReleaseIP(ctx, allocatedIP, phase1VPCID, inst.ID); relErr != nil {
				log.Error("rollback: IP release failed", "error", relErr)
			}
			return h.failInstance(ctx, inst, fmt.Errorf("step7 readiness: %w", err))
		}
	}
	log.Info("step7: VM ready")

	// ── Step 8: Transition provisioning → running ────────────────────────────
	// usage.start coupled to RUNNING write per LIFECYCLE_STATE_MACHINE_V1 §6.
	if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "provisioning", "running", inst.Version); err != nil {
		return fmt.Errorf("step8 transition to running: %w", err)
	}
	// Update NIC status to "attached" now that the VM is live on the host.
	// Phase 1 classic instances (no NIC row) hit the nil guard and skip safely.
	// Source: VM-P2A-S2 audit finding R3; P2_VPC_NETWORK_CONTRACT §5.
	nic, _ := h.deps.Store.GetPrimaryNetworkInterfaceByInstance(ctx, inst.ID)
	if nic != nil {
		if err := h.deps.Store.UpdateNetworkInterfaceStatus(ctx, nic.ID, "attached"); err != nil {
			// Non-fatal: instance is running. Reconciler can repair NIC status drift.
			log.Error("step8: UpdateNetworkInterfaceStatus(attached) failed — non-fatal", "nic_id", nic.ID, "error", err)
		} else {
			log.Info("step8: NIC re-attached", "nic_id", nic.ID)
		}
	}

	h.writeEvent(ctx, inst.ID, db.EventInstanceProvisioningDone, "Re-provisioning complete")
	h.writeEvent(ctx, inst.ID, db.EventUsageStart, "Usage billing started")
	log.Info("INSTANCE_START: completed — instance running", "ip", allocatedIP)
	return nil
}

// failInstance transitions to failed, writes an event, returns the cause.
// Mirrors create.go's failInstance pattern exactly.
func (h *StartHandler) failInstance(ctx context.Context, inst *db.InstanceRow, cause error) error {
	if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, inst.VMState, "failed", inst.Version); err != nil {
		h.log.Error("failInstance: could not set failed", "instance_id", inst.ID, "error", err)
	}
	inst.VMState = "failed"
	h.writeEvent(ctx, inst.ID, db.EventInstanceFailure, cause.Error())
	return cause
}

func (h *StartHandler) writeEvent(ctx context.Context, instanceID, eventType, msg string) {
	_ = h.deps.Store.InsertEvent(ctx, &db.EventRow{
		ID:         idgen.New(idgen.PrefixEvent),
		InstanceID: instanceID,
		EventType:  eventType,
		Message:    msg,
		Actor:      "system",
	})
}

// SetRuntimeFactory overrides the runtime client factory. Used by integration tests.
func (h *StartHandler) SetRuntimeFactory(f func(hostID, address string) RuntimeClient) {
	h.runtimeFactory = f
}

// SetReadinessFn overrides the readiness check function. Used by integration tests.
func (h *StartHandler) SetReadinessFn(f func(ctx context.Context, ip string, timeout time.Duration) error) {
	h.readinessFn = f
}
