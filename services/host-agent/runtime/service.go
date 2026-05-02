package runtime

// service.go — RuntimeService gRPC server implementation on the Host Agent.
//
// Source: RUNTIMESERVICE_GRPC_V1 §7, IMPLEMENTATION_PLAN_V1 §27-29.
//
// This file implements the RuntimeService interface using the three managers:
//   - FirecrackerManager  (start_vm, stop_vm, delete_vm process)
//   - RootfsManager       (materialise, delete qcow2 overlay)
//   - NetworkManager      (TAP device, iptables NAT)
//
// All RPCs are idempotent. The host agent is the sole runtime authority —
// the control plane never calls Firecracker or ip(8) directly.
//
// Authentication: mTLS enforced at the gRPC server level (see host-agent/main.go).
// The CN of the client certificate is the worker's service identity.
//
// gRPC transport: this implementation uses the proto definitions from
// packages/contracts/runtimev1/runtime.proto. Until protoc-generated code is
// wired in, the service is implemented against the hand-written interface below
// so the rest of the codebase can compile and the worker can call it.
//
// To replace with generated code:
//   1. Run protoc on runtime.proto to generate runtimev1/runtime.pb.go.
//   2. Replace RuntimeServiceServer interface with the generated one.
//   3. Register with grpc.NewServer() in main.go.

import (
	"context"
	"fmt"
	"log/slog"
)

// ── Hand-written proto types (until protoc is wired in) ──────────────────────
// These types mirror packages/contracts/runtimev1/runtime.proto exactly.
// Replace with generated types once protoc is available.

type NetworkConfig struct {
	PrivateIP  string
	PublicIP   string
	TapDevice  string
	MacAddress string
}

type CreateInstanceRequest struct {
	InstanceID     string        `json:"instance_id"`
	ImageURL       string        `json:"image_url"` // object storage URL for base image
	InstanceTypeID string        `json:"instance_type_id"`
	CPUCores       int32         `json:"cpu_cores"`
	MemoryMB       int32         `json:"memory_mb"`
	DiskGB         int32         `json:"disk_gb"`
	RootfsPath     string        `json:"rootfs_path"` // NFS path for qcow2 CoW overlay
	Network        NetworkConfig `json:"network"`
	SSHPublicKey   string        `json:"ssh_public_key"`
}

type CreateInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "CREATED"
}

type StartInstanceRequest struct {
	InstanceID string `json:"instance_id"`
}

type StartInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "RUNNING"
}

type StopInstanceRequest struct {
	InstanceID     string `json:"instance_id"`
	TimeoutSeconds int32  `json:"timeout_seconds"`
}

type StopInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "STOPPED"
}

type DeleteInstanceRequest struct {
	InstanceID     string `json:"instance_id"`
	DeleteRootDisk bool   `json:"delete_root_disk"` // Phase 1: always true
}

type DeleteInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "DELETED"
}

type ListInstancesRequest struct{}

type InstanceStatus struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	HostPID    int32  `json:"host_pid"`
}

type ListInstancesResponse struct {
	Instances []InstanceStatus
}

// ── RuntimeService implementation ────────────────────────────────────────────

// RuntimeService implements the gRPC RuntimeService interface on the Host Agent.
// It orchestrates VMRuntime, RootfsManager, and NetworkManager.
type RuntimeService struct {
	vm  VMRuntime
	rfs *RootfsManager
	net *NetworkManager
	log *slog.Logger
}

// NewRuntimeService constructs a RuntimeService with the given managers.
func NewRuntimeService(
	vm VMRuntime,
	rfs *RootfsManager,
	netMgr *NetworkManager,
	log *slog.Logger,
) *RuntimeService {
	return &RuntimeService{vm: vm, rfs: rfs, net: netMgr, log: log}
}

// CreateInstance materialises the rootfs and configures network, then boots the VM.
//
// Sequence (source: RUNTIMESERVICE_GRPC_V1 §7):
//  1. Materialize rootfs (qcow2 CoW overlay) — idempotent.
//  2. Create TAP device — idempotent.
//  3. Program NAT rules — idempotent.
//  4. Launch Firecracker process — idempotent.
//
// On any step failure, RollbackCreate is called to clean up in reverse order.
// Idempotent: re-running after a partial failure resumes from the first step
// that is not yet complete (each step is individually idempotent).
func (s *RuntimeService) CreateInstance(ctx context.Context, req *CreateInstanceRequest) (*CreateInstanceResponse, error) {
	s.log.Info("CreateInstance called",
		"instance_id", req.InstanceID,
		"image_url", req.ImageURL,
		"cpu", req.CPUCores,
		"mem_mb", req.MemoryMB,
	)

	// Step 1: Materialise rootfs.
	rootfsPath, err := s.rfs.Materialize(ctx, req.InstanceID, req.ImageURL)
	if err != nil {
		return nil, fmt.Errorf("CreateInstance: rootfs: %w", err)
	}

	// Step 2: Create TAP device.
	tapDev, err := s.net.CreateTAP(ctx, req.InstanceID, req.Network.MacAddress, "")
	if err != nil {
		// Rollback: rootfs only (TAP not created).
		_ = s.rfs.Delete(req.InstanceID)
		return nil, fmt.Errorf("CreateInstance: TAP: %w", err)
	}

	// Step 3: Program NAT rules.
	if err := s.net.ProgramNAT(ctx, req.InstanceID, req.Network.PrivateIP, req.Network.PublicIP); err != nil {
		// Rollback: TAP + rootfs.
		_ = s.net.DeleteTAP(ctx, req.InstanceID, "")
		_ = s.rfs.Delete(req.InstanceID)
		return nil, fmt.Errorf("CreateInstance: NAT: %w", err)
	}

	// Step 4: Launch VM via the configured runtime backend.
	spec := InstanceSpec{
		InstanceID:   req.InstanceID,
		CPUCores:     req.CPUCores,
		MemoryMB:     req.MemoryMB,
		RootfsPath:   rootfsPath,
		TapDevice:    tapDev,
		MacAddress:   req.Network.MacAddress,
		PrivateIP:    req.Network.PrivateIP,
		SSHPublicKey: req.SSHPublicKey,
	}
	if _, err := s.vm.Create(ctx, spec); err != nil {
		// Rollback: NAT + TAP + rootfs.
		_ = s.net.RemoveNAT(ctx, req.InstanceID, req.Network.PrivateIP, req.Network.PublicIP)
		_ = s.net.DeleteTAP(ctx, req.InstanceID, "")
		_ = s.rfs.Delete(req.InstanceID)
		return nil, fmt.Errorf("CreateInstance: start VM: %w", err)
	}

	s.log.Info("CreateInstance completed",
		"instance_id", req.InstanceID,
		"private_ip", req.Network.PrivateIP,
		"tap", tapDev,
		"rootfs", rootfsPath,
	)
	return &CreateInstanceResponse{InstanceID: req.InstanceID, State: "RUNNING"}, nil
}

// StartInstance starts a previously created (but stopped) VM.
// Phase 1: a stopped instance loses its TAP and rootfs (stop = delete resources).
// StartInstance re-materialises them and relaunches the VM.
// Source: 04-02-lifecycle-action-flows.md §INSTANCE_START.
func (s *RuntimeService) StartInstance(ctx context.Context, req *StartInstanceRequest) (*StartInstanceResponse, error) {
	s.log.Info("StartInstance called", "instance_id", req.InstanceID)
	// Phase 1: StartInstance is called by the worker's INSTANCE_START handler
	// which re-runs the full CreateInstance provisioning sequence on a new host.
	// The host agent's StartInstance RPC is therefore equivalent to CreateInstance
	// without the initial rootfs / network parameters (those come from the job).
	// For the M2 vertical slice, the worker calls CreateInstance directly.
	// This RPC is a stub for future use.
	return &StartInstanceResponse{InstanceID: req.InstanceID, State: "RUNNING"}, nil
}

// StopInstance gracefully stops a running VM.
// Phase 1 contract: stop = VM process terminated; TAP and rootfs are released by
// the INSTANCE_STOP worker handler (not here — the Host Agent only stops the process).
// Source: RUNTIMESERVICE_GRPC_V1 §StopInstance.
func (s *RuntimeService) StopInstance(ctx context.Context, req *StopInstanceRequest) (*StopInstanceResponse, error) {
	s.log.Info("StopInstance called", "instance_id", req.InstanceID)
	if err := s.vm.Stop(ctx, req.InstanceID); err != nil {
		return nil, fmt.Errorf("StopInstance: %w", err)
	}
	return &StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"}, nil
}

// DeleteInstance destroys all VM resources: process, rootfs, TAP, NAT.
// Executes in reverse-allocation order:
//
//	process kill → TAP delete → NAT remove → rootfs delete
//
// Source: RUNTIMESERVICE_GRPC_V1 §DeleteInstance, IMPLEMENTATION_PLAN_V1 rollback order.
func (s *RuntimeService) DeleteInstance(ctx context.Context, req *DeleteInstanceRequest) (*DeleteInstanceResponse, error) {
	s.log.Info("DeleteInstance called", "instance_id", req.InstanceID, "delete_root_disk", req.DeleteRootDisk)

	// 1. Kill VM process (idempotent).
	if err := s.vm.Delete(ctx, req.InstanceID); err != nil {
		s.log.Warn("DeleteInstance: DeleteVM partial failure",
			"instance_id", req.InstanceID, "error", err)
		// Continue — do not abort cleanup.
	}

	// 2. Delete TAP device (idempotent).
	if err := s.net.DeleteTAP(ctx, req.InstanceID, ""); err != nil {
		s.log.Warn("DeleteInstance: DeleteTAP partial failure",
			"instance_id", req.InstanceID, "error", err)
	}

	// 3. Remove NAT rules.
	// Note: privateIP and publicIP are not known at this layer — the worker
	// passes them via the delete job payload and calls RemoveNAT separately
	// before issuing DeleteInstance to the host agent.
	// The Host Agent only handles process + TAP + rootfs.

	// 4. Delete rootfs overlay (Phase 1: always delete_on_termination=true).
	if req.DeleteRootDisk {
		if err := s.rfs.Delete(req.InstanceID); err != nil {
			return nil, fmt.Errorf("DeleteInstance: rootfs delete: %w", err)
		}
	}

	s.log.Info("DeleteInstance completed", "instance_id", req.InstanceID)
	return &DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"}, nil
}

// ListInstances returns the runtime status of all instances whose PID files exist.
// Used by the reconciler to detect drift between DB state and hypervisor state.
// Source: RUNTIMESERVICE_GRPC_V1 §ListInstances.
func (s *RuntimeService) ListInstances(_ context.Context, _ *ListInstancesRequest) (*ListInstancesResponse, error) {
	infos, err := s.vm.List(context.Background())
	if err != nil {
		return nil, fmt.Errorf("ListInstances: %w", err)
	}
	var statuses []InstanceStatus
	for _, info := range infos {
		statuses = append(statuses, InstanceStatus{
			InstanceID: info.InstanceID,
			State:      info.State,
			HostPID:    info.PID,
		})
	}
	return &ListInstancesResponse{Instances: statuses}, nil
}
