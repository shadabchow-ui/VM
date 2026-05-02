package runtime

// vm_runtime.go — VMRuntime interface and shared types for VM process lifecycle.
//
// The VMRuntime interface abstracts hypervisor-specific VM lifecycle (create,
// start, stop, reboot, delete, inspect, list). Implementations include:
//   - FirecrackerManager (existing Firecracker backend)
//   - QemuManager (QEMU/KVM backend)
//   - FakeRuntime (deterministic test backend)
//
// The interface is consumed by RuntimeService, which orchestrates rootfs
// materialisation and network configuration around the VM process.

import "context"

// VMRuntime defines the contract for VM process lifecycle management.
// All methods are idempotent. The host agent uses this interface to manage
// VM processes without coupling to a specific hypervisor implementation.
type VMRuntime interface {
	// Create provisions VM process artifacts (PID, socket, console) and starts the VM.
	// Idempotent: if the instance is already running, returns the existing state.
	Create(ctx context.Context, spec InstanceSpec) (*RuntimeInfo, error)

	// Start starts a previously created (but stopped) VM process.
	// For Phase 1, this is equivalent to Create (full re-provision).
	Start(ctx context.Context, instanceID string) error

	// Stop gracefully stops a running VM process.
	// Idempotent: if the process is not running, returns nil.
	Stop(ctx context.Context, instanceID string) error

	// Reboot reboots a running VM process (stop + start internally).
	Reboot(ctx context.Context, instanceID string) error

	// Delete destroys all VM process artifacts (PID file, socket, console log).
	// Idempotent: if no artifacts remain, returns nil.
	Delete(ctx context.Context, instanceID string) error

	// Inspect returns the current runtime state of an instance by its ID.
	Inspect(ctx context.Context, instanceID string) (*RuntimeInfo, error)

	// List returns the runtime state of all managed instances on this host.
	List(ctx context.Context) ([]RuntimeInfo, error)

	// DataRoot returns the configured data root directory for this runtime.
	DataRoot() string
}

// ExtraDisk describes an additional block device to attach to a VM at boot.
// A volume attachment maps to exactly one ExtraDisk.
// The HostPath is derived from the volume's storage_path under the storage root.
type ExtraDisk struct {
	DiskID     string // volume ID
	HostPath   string // host file path (qcow2/raw)
	DeviceName string // guest device name e.g. /dev/vdb
}

// InstanceSpec holds all parameters needed to launch a VM process.
// This is the canonical input type for VMRuntime.Create.
type InstanceSpec struct {
	InstanceID   string
	CPUCores     int32
	MemoryMB     int32
	RootfsPath   string // qcow2 overlay path for root disk
	KernelPath   string // optional kernel image path (required for Firecracker)
	TapDevice    string // TAP device name
	MacAddress   string
	PrivateIP    string
	SSHPublicKey string
	ExtraDisks   []ExtraDisk // additional block devices from attached volumes
}

// RuntimeInfo holds the current runtime state of a single VM instance.
type RuntimeInfo struct {
	InstanceID string
	State      string // "RUNNING", "STOPPED", "DELETED"
	PID        int32
	DataDir    string // per-instance subdirectory under data root
}
