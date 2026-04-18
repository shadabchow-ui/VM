package handlers

// create.go — INSTANCE_CREATE job handler.
//
// Source: IMPLEMENTATION_PLAN_V1 §37-38, 04-01-create-instance-flow.md,
//         04-04-provisioning-failure-handling-and-rollback.md.
//
// Provisioning sequence (exact order — R-06):
//  1. DB: transition requested → provisioning. Write provisioning.start event.
//  2. Scheduler: select host (first-fit by free CPU from InstanceStore.GetAvailableHosts).
//  3. DB: assign host_id.
//  3b. DB: create root_disks record (status=CREATING). Idempotent: skip if already exists.
//       Source: 06-01-root-disk-model-and-persistence-semantics.md (root disk object model),
//               P2_VOLUME_MODEL.md §1 (Phase 1: delete_on_termination always true).
//  4. Network: allocate IP (SELECT FOR UPDATE SKIP LOCKED via network controller).
//  5. Host Agent: CreateInstance (rootfs + TAP + NAT + Firecracker).
//  6. Readiness: wait for SSH port (up to 120s). Mockable via readinessFn for tests.
//  7. DB: transition provisioning → running. Update root disk status to ATTACHED.
//         Write usage.start + provisioning.done events.
//
// Failure at any step triggers rollback in reverse order.
// All Host Agent operations are idempotent — safe to replay on retry.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

const (
	readinessTimeout = 120 * time.Second
	readinessPoll    = 3 * time.Second
	sshPort          = "22"
	phase1VPCID      = "00000000-0000-0000-0000-000000000001"

	// phase1StoragePoolID is the fixed NFS pool ID for Phase 1.
	// All root disks are stored on the shared NFS export.
	// Source: 06-01-root-disk-model-and-persistence-semantics.md §Phase 1 Storage Backend.
	phase1StoragePoolID = "00000000-0000-0000-0000-000000000002"
)

// rootDiskIDFromInstance derives a deterministic disk ID from the instance ID.
// Phase 1: exactly one root disk per instance. Deterministic ID makes
// CreateRootDisk idempotent on job retry.
// Format: "disk_" + instance_id (matches existing test data conventions).
func rootDiskIDFromInstance(instanceID string) string {
	return "disk_" + instanceID
}

// CreateHandler handles INSTANCE_CREATE jobs.
type CreateHandler struct {
	deps *Deps
	log  *slog.Logger
	// runtimeFactory is overridable for tests.
	runtimeFactory func(hostID, address string) RuntimeClient
	// readinessFn is overridable for tests. Default: waitForSSH.
	readinessFn func(ctx context.Context, ip string, timeout time.Duration) error
}

// NewCreateHandler constructs a CreateHandler with production defaults.
func NewCreateHandler(deps *Deps, log *slog.Logger) *CreateHandler {
	h := &CreateHandler{deps: deps, log: log}
	h.runtimeFactory = func(hostID, address string) RuntimeClient {
		return deps.Runtime(hostID, address)
	}
	h.readinessFn = waitForSSH
	return h
}

// Execute runs the full provisioning sequence. Idempotent: each step checks
// current state before acting.
func (h *CreateHandler) Execute(ctx context.Context, job *db.JobRow) error {
	log := h.log.With("job_id", job.ID, "instance_id", job.InstanceID)
	log.Info("INSTANCE_CREATE: starting provisioning")

	// ── Step 1: Load instance & transition to provisioning ───────────────────
	inst, err := h.deps.Store.GetInstanceByID(ctx, job.InstanceID)
	if err != nil {
		return fmt.Errorf("step1 load instance: %w", err)
	}
	if inst.VMState != "requested" && inst.VMState != "provisioning" {
		return fmt.Errorf("step1: unexpected state %q", inst.VMState)
	}
	if inst.VMState == "requested" {
		if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "requested", "provisioning", inst.Version); err != nil {
			return fmt.Errorf("step1 state transition: %w", err)
		}
		inst.Version++
		inst.VMState = "provisioning"
	}
	h.writeEvent(ctx, inst.ID, db.EventInstanceProvisioningStart, "Provisioning started")
	log.Info("step1: instance provisioning")

	// ── Step 2: Select host ───────────────────────────────────────────────────
	hosts, err := h.deps.Store.GetAvailableHosts(ctx)
	if err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step2 get hosts: %w", err))
	}
	if len(hosts) == 0 {
		return h.failInstance(ctx, inst, fmt.Errorf("step2: no available hosts"))
	}
	selectedHost := hosts[0] // first-fit; hosts sorted by free CPU desc
	log.Info("step2: host selected", "host_id", selectedHost.ID)

	// ── Step 3: Assign host ───────────────────────────────────────────────────
	if inst.HostID == nil {
		if err := h.deps.Store.AssignHost(ctx, inst.ID, selectedHost.ID, inst.Version); err != nil {
			return h.failInstance(ctx, inst, fmt.Errorf("step3 assign host: %w", err))
		}
		inst.Version++
		inst.HostID = &selectedHost.ID
	}
	log.Info("step3: host assigned", "host_id", selectedHost.ID)

	// ── Step 3b: Create root disk record ──────────────────────────────────────
	// A root_disks row is inserted here so the control plane tracks the disk
	// as a first-class object independent of the instance row.
	// Phase 1 contract: delete_on_termination is always true.
	// Source: 06-01-root-disk-model-and-persistence-semantics.md §Root Disk Object Model,
	//         P2_VOLUME_MODEL.md §1.
	diskID := rootDiskIDFromInstance(inst.ID)
	diskSizeGB := int(shapeDiskGB(inst.InstanceTypeID))
	storagePath := fmt.Sprintf("nfs://filer/vol/%s.qcow2", diskID)

	// Idempotent: if the disk row already exists (job retry), skip insert.
	existingDisk, err := h.deps.Store.GetRootDiskByInstanceID(ctx, inst.ID)
	if err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step3b check existing disk: %w", err))
	}
	if existingDisk == nil {
		instIDCopy := inst.ID
		diskRow := &db.RootDiskRow{
			DiskID:              diskID,
			InstanceID:          &instIDCopy,
			SourceImageID:       inst.ImageID,
			StoragePoolID:       phase1StoragePoolID,
			StoragePath:         storagePath,
			SizeGB:              diskSizeGB,
			DeleteOnTermination: true, // Phase 1 immutable default
			Status:              db.RootDiskStatusCreating,
		}
		if err := h.deps.Store.CreateRootDisk(ctx, diskRow); err != nil {
			return h.failInstance(ctx, inst, fmt.Errorf("step3b create root disk: %w", err))
		}
		log.Info("step3b: root disk record created", "disk_id", diskID, "storage_path", storagePath)
	} else {
		diskID = existingDisk.DiskID
		storagePath = existingDisk.StoragePath
		log.Info("step3b: root disk record already exists (retry)", "disk_id", diskID)
	}

	// ── Step 4: Allocate IP ───────────────────────────────────────────────────
	vpcID := phase1VPCID
	allocatedIP, err := h.deps.Network.AllocateIP(ctx, vpcID, inst.ID)
	if err != nil {
		return h.failInstance(ctx, inst, fmt.Errorf("step4 allocate IP: %w", err))
	}
	h.writeEvent(ctx, inst.ID, db.EventIPAllocated, "IP allocated: "+allocatedIP)
	log.Info("step4: IP allocated", "ip", allocatedIP)

	// ── Step 5: CreateInstance on Host Agent ──────────────────────────────────
	hostAddr := selectedHost.ID + ":50051"
	rtClient := h.runtimeFactory(selectedHost.ID, hostAddr)

	createReq := &runtimeclient.CreateInstanceRequest{
		InstanceID:     inst.ID,
		ImageURL:       imageURLFromID(inst.ImageID),
		InstanceTypeID: inst.InstanceTypeID,
		CPUCores:       shapeCPU(inst.InstanceTypeID),
		MemoryMB:       shapeMemMB(inst.InstanceTypeID),
		DiskGB:         shapeDiskGB(inst.InstanceTypeID),
		// RootfsPath derives from disk storage path (NFS-backed qcow2).
		// Source: 06-01-root-disk-model-and-persistence-semantics.md §CoW Implementation.
		RootfsPath: fmt.Sprintf("/mnt/nfs/vols/%s.qcow2", diskID),
		Network: runtimeclient.NetworkConfig{
			PrivateIP:  allocatedIP,
			TapDevice:  "tap-" + inst.ID[:8],
			MacAddress: deriveMACAddress(inst.ID),
		},
	}
	if _, err := rtClient.CreateInstance(ctx, createReq); err != nil {
		// Rollback: release IP, then fail instance.
		// Root disk record is left as CREATING; it will be cleaned up by
		// the reconciler's orphan GC or on next delete job.
		if relErr := h.deps.Network.ReleaseIP(ctx, allocatedIP, vpcID, inst.ID); relErr != nil {
			log.Error("rollback: IP release failed", "error", relErr)
		}
		return h.failInstance(ctx, inst, fmt.Errorf("step5 CreateInstance: %w", err))
	}
	log.Info("step5: VM running on host")

	// ── Step 6: Wait for readiness ────────────────────────────────────────────
	if os.Getenv("READINESS_DRY_RUN") == "true" {
		log.Warn("READINESS_DRY_RUN=true: skipping SSH readiness check")
	} else {
		if err := h.readinessFn(ctx, allocatedIP, readinessTimeout); err != nil {
			h.rollbackCreate(ctx, log, rtClient, inst.ID, allocatedIP, vpcID)
			return h.failInstance(ctx, inst, fmt.Errorf("step6 readiness: %w", err))
		}
	}
	log.Info("step6: VM ready")

	// ── Step 7: Transition to running + mark disk ATTACHED ───────────────────
	if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, "provisioning", "running", inst.Version); err != nil {
		return fmt.Errorf("step7 transition to running: %w", err)
	}
	// Update root disk status to ATTACHED now that the VM is live.
	// Source: 06-01-root-disk-model-and-persistence-semantics.md §status values.
	if err := h.deps.Store.UpdateRootDiskStatus(ctx, diskID, db.RootDiskStatusAttached); err != nil {
		// Non-fatal: instance is running. Log and continue.
		// The reconciler can repair status drift.
		log.Error("step7: UpdateRootDiskStatus to ATTACHED failed — non-fatal", "disk_id", diskID, "error", err)
	}
	// Update NIC status to "attached" now that the VM is live on the host.
	// Phase 1 classic instances (no NIC row) hit the nil guard and skip safely.
	// Source: VM-P2A-S2 audit finding R3; P2_VPC_NETWORK_CONTRACT §5.
	nic, _ := h.deps.Store.GetPrimaryNetworkInterfaceByInstance(ctx, inst.ID)
	if nic != nil {
		if err := h.deps.Store.UpdateNetworkInterfaceStatus(ctx, nic.ID, "attached"); err != nil {
			// Non-fatal: instance is running. Reconciler can repair NIC status drift.
			log.Error("step7: UpdateNetworkInterfaceStatus(attached) failed — non-fatal", "nic_id", nic.ID, "error", err)
		} else {
			log.Info("step7: NIC attached", "nic_id", nic.ID)
		}
	}

	h.writeEvent(ctx, inst.ID, db.EventInstanceProvisioningDone, "Provisioning complete")
	h.writeEvent(ctx, inst.ID, db.EventUsageStart, "Usage billing started")
	log.Info("INSTANCE_CREATE: completed — instance running", "ip", allocatedIP, "disk_id", diskID)
	return nil
}

// rollbackCreate cleans up in reverse-allocation order. All steps are idempotent.
// Source: R-06, IMPLEMENTATION_PLAN_V1 §38.
func (h *CreateHandler) rollbackCreate(ctx context.Context, log *slog.Logger, rtClient RuntimeClient, instanceID, allocatedIP, vpcID string) {
	log.Warn("rollback: starting")
	if _, err := rtClient.DeleteInstance(ctx, &runtimeclient.DeleteInstanceRequest{
		InstanceID: instanceID, DeleteRootDisk: true,
	}); err != nil {
		log.Error("rollback: DeleteInstance failed", "error", err)
	}
	if allocatedIP != "" {
		if err := h.deps.Network.ReleaseIP(ctx, allocatedIP, vpcID, instanceID); err != nil {
			log.Error("rollback: ReleaseIP failed", "error", err)
		}
	}
	log.Warn("rollback: complete")
}

// failInstance transitions an instance to failed, writes an event, returns the cause.
func (h *CreateHandler) failInstance(ctx context.Context, inst *db.InstanceRow, cause error) error {
	if err := h.deps.Store.UpdateInstanceState(ctx, inst.ID, inst.VMState, "failed", inst.Version); err != nil {
		h.log.Error("failInstance: could not set failed", "instance_id", inst.ID, "error", err)
	}
	inst.VMState = "failed"
	h.writeEvent(ctx, inst.ID, db.EventInstanceFailure, cause.Error())
	return cause
}

func (h *CreateHandler) writeEvent(ctx context.Context, instanceID, eventType, msg string) {
	_ = h.deps.Store.InsertEvent(ctx, &db.EventRow{
		ID:         idgen.New(idgen.PrefixEvent),
		InstanceID: instanceID,
		EventType:  eventType,
		Message:    msg,
		Actor:      "system",
	})
}

// waitForSSH polls the SSH port until it accepts TCP connections.
// Source: 04-03-bootstrap-initialization-and-readiness-signaling.md.
func waitForSSH(ctx context.Context, ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(ip, sshPort)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(readinessPoll)
	}
	return fmt.Errorf("SSH port on %s did not open within %s", ip, timeout)
}

// deriveMACAddress creates a locally-administered MAC from the instance ID.
func deriveMACAddress(instanceID string) string {
	s := instanceID
	if len(s) < 10 {
		s += "0000000000"
	}
	return fmt.Sprintf("02:%s:%s:%s:%s:%s", s[0:2], s[2:4], s[4:6], s[6:8], s[8:10])
}

// imageURLFromID maps image UUID → object storage URL.
func imageURLFromID(imageID string) string {
	known := map[string]string{
		"00000000-0000-0000-0000-000000000010": "object://images/ubuntu-22.04-base.qcow2",
		"00000000-0000-0000-0000-000000000011": "object://images/debian-12-base.qcow2",
	}
	if url, ok := known[imageID]; ok {
		return url
	}
	return "object://images/" + imageID + ".qcow2"
}

func shapeCPU(t string) int32 {
	return map[string]int32{"c1.small": 2, "c1.medium": 4, "c1.large": 8, "c1.xlarge": 16}[t]
}
func shapeMemMB(t string) int32 {
	return map[string]int32{"c1.small": 4096, "c1.medium": 8192, "c1.large": 16384, "c1.xlarge": 32768}[t]
}
func shapeDiskGB(t string) int32 {
	return map[string]int32{"c1.small": 50, "c1.medium": 100, "c1.large": 200, "c1.xlarge": 500}[t]
}

// CreateJobPayload is optional JSON in the job for INSTANCE_CREATE.
type CreateJobPayload struct {
	SSHPublicKey string `json:"ssh_public_key"`
}

func ParseCreatePayload(raw []byte) (*CreateJobPayload, error) {
	if len(raw) == 0 {
		return &CreateJobPayload{}, nil
	}
	var p CreateJobPayload
	return &p, json.Unmarshal(raw, &p)
}

// SetRuntimeFactory overrides the runtime client factory. Used by integration tests.
func (h *CreateHandler) SetRuntimeFactory(f func(hostID, address string) RuntimeClient) {
	h.runtimeFactory = f
}

// SetReadinessFn overrides the readiness check function. Used by integration tests.
func (h *CreateHandler) SetReadinessFn(f func(ctx context.Context, ip string, timeout time.Duration) error) {
	h.readinessFn = f
}
