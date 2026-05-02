package runtime

// config.go — Runtime configuration: data root, runtime backend selection, defaults.
//
// Source: VM Job 2 — configurable runtime data root.
//
// Environment variables:
//   VM_PLATFORM_DATA_ROOT   — root directory for all runtime artifacts
//                            (default: /var/lib/compute-platform/instances)
//   VM_PLATFORM_RUNTIME     — runtime backend selection: "firecracker", "qemu", "fake"
//                            (default: "firecracker")
//   FIRECRACKER_DRY_RUN     — if "true", firecracker Create returns synthetic state
//   QEMU_DRY_RUN            — if "true", qemu Create returns synthetic state
//   NETWORK_DRY_RUN         — if "true", network operations are no-ops

import "os"

const (
	// DefaultDataRoot is the default directory for all runtime instance artifacts.
	DefaultDataRoot = "/var/lib/compute-platform/instances"

	// RuntimeFirecracker names the Firecracker microVM backend.
	RuntimeFirecracker = "firecracker"

	// RuntimeQEMU names the QEMU/KVM backend.
	RuntimeQEMU = "qemu"

	// RuntimeFake names the fake/test backend (no real VM launched).
	RuntimeFake = "fake"
)

// RuntimeConfig holds all configuration for the runtime subsystem.
type RuntimeConfig struct {
	DataRoot   string // root directory for runtime artifacts
	Backend    string // "firecracker", "qemu", or "fake"
	SocketDir  string // directory for Firecracker API sockets
	PIDDir     string // directory for PID files
	KernelPath string // path to kernel image (Firecracker only)
	NFSRoot    string // NFS mount root for qcow2 overlays
}

// DefaultRuntimeConfig returns a RuntimeConfig populated from environment
// variables with sensible defaults.
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		DataRoot:   envDefault("VM_PLATFORM_DATA_ROOT", DefaultDataRoot),
		Backend:    envDefault("VM_PLATFORM_RUNTIME", RuntimeFirecracker),
		SocketDir:  envDefault("FIRECRACKER_SOCKET_DIR", "/run/firecracker"),
		PIDDir:     envDefault("FIRECRACKER_PID_DIR", "/run/firecracker/pids"),
		KernelPath: envDefault("KERNEL_PATH", "/opt/firecracker/vmlinux"),
		NFSRoot:    envDefault("NFS_ROOT", "/mnt/nfs/vols"),
	}
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
