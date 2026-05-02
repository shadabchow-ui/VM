package runtime

// firecracker.go — Firecracker microVM launch and lifecycle management.
//
// Source: RUNTIMESERVICE_GRPC_V1 §7 (CreateInstance sequence),
//         05-01-runtime-abstraction-strategy.md,
//         IMPLEMENTATION_PLAN_V1 §27, §28, §29.
//
// Phase 1 sole runtime target: Firecracker microVMs.
//
// VM lifecycle primitives:
//   StartVM(req)        — launch a Firecracker process via the Unix socket API.
//   StopVM(instanceID)  — graceful ACPI shutdown, then SIGKILL on timeout.
//   DeleteVM(instanceID)— assert the process is gone; no-op if already absent.
//
// State is tracked in a pid-file at <PID_DIR>/<instanceID>.pid.
// All primitives are idempotent:
//   - StartVM on an already-running instance → returns existing PID, no new process.
//   - StopVM on an absent process → no-op.
//   - DeleteVM on an absent process → no-op.
//
// Required on host:
//   - firecracker binary on PATH
//   - jailer or direct execution (Phase 1: direct, no jailer)
//   - Linux kernel image at KERNEL_PATH env
//
// Socket path: <SOCKET_DIR>/<instanceID>.sock  (default /run/firecracker)
// PID file:    <PID_DIR>/<instanceID>.pid      (default /run/firecracker/pids)

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultSocketDir  = "/run/firecracker"
	defaultPIDDir     = "/run/firecracker/pids"
	defaultKernelPath = "/opt/firecracker/vmlinux"

	// ACPI shutdown grace period before SIGKILL.
	acpiGracePeriod = 30 * time.Second

	// Firecracker API timeout for individual HTTP calls.
	fcAPITimeout = 10 * time.Second

	// How long to wait for the Firecracker API socket to appear after launch.
	socketReadyTimeout = 5 * time.Second
)

// StartVMRequest holds all parameters needed to launch a Firecracker VM.
// Corresponds to CreateInstanceRequest in the gRPC proto after field mapping.
type StartVMRequest struct {
	InstanceID   string
	KernelPath   string // path to vmlinux on host (defaults to KERNEL_PATH env)
	RootfsPath   string // qcow2 overlay path (from RootfsManager.OverlayPath)
	CPUCores     int32
	MemoryMB     int32
	TapDevice    string // TAP device name (from NetworkManager.CreateTAP)
	MacAddress   string
	PrivateIP    string // for kernel boot args
	SSHPublicKey string // injected via metadata service; not directly used here
}

// dryRunPID is the synthetic PID returned by StartVM when FIRECRACKER_DRY_RUN=true.
// Chosen to be outside the Linux PID range (max 4194304) so callers can never
// mistake it for a real process. All other lifecycle operations treat an absent
// process as a no-op (idempotent), so this value is safe to store in memory.
const dryRunPID = 999999999

// FirecrackerManager manages Firecracker process lifecycle.
type FirecrackerManager struct {
	socketDir  string
	pidDir     string
	kernelPath string
	dryRun     bool // FIRECRACKER_DRY_RUN=true: skip all runtime-dir and binary side effects
	log        *slog.Logger
}

// NewFirecrackerManager constructs a FirecrackerManager.
// Empty strings fall back to environment variables, then hard-coded defaults.
func NewFirecrackerManager(socketDir, pidDir, kernelPath string, log *slog.Logger) *FirecrackerManager {
	if socketDir == "" {
		socketDir = os.Getenv("FIRECRACKER_SOCKET_DIR")
	}
	if socketDir == "" {
		socketDir = defaultSocketDir
	}
	if pidDir == "" {
		pidDir = os.Getenv("FIRECRACKER_PID_DIR")
	}
	if pidDir == "" {
		pidDir = defaultPIDDir
	}
	if kernelPath == "" {
		kernelPath = os.Getenv("KERNEL_PATH")
	}
	if kernelPath == "" {
		kernelPath = defaultKernelPath
	}
	return &FirecrackerManager{
		socketDir:  socketDir,
		pidDir:     pidDir,
		kernelPath: kernelPath,
		dryRun:     os.Getenv("FIRECRACKER_DRY_RUN") == "true",
		log:        log,
	}
}

func (f *FirecrackerManager) socketPath(instanceID string) string {
	return filepath.Join(f.socketDir, instanceID+".sock")
}

func (f *FirecrackerManager) pidFilePath(instanceID string) string {
	return filepath.Join(f.pidDir, instanceID+".pid")
}

// StartVM launches a Firecracker microVM for the given instance.
// Idempotent: if a process for this instance is already running, returns its PID.
//
// Sequence:
//  1. Check PID file → already running → return existing PID.
//  2. Ensure directories exist.
//  3. Remove stale socket if present.
//  4. Launch firecracker --api-sock <socket> as a background process.
//  5. Wait for socket to appear (up to socketReadyTimeout).
//  6. Configure VM via Firecracker API (kernel, drives, net interface, machine config).
//  7. Issue InstanceStart action.
//  8. Write PID file.
//
// Source: IMPLEMENTATION_PLAN_V1 §27 (start_vm: Firecracker launch).
func (f *FirecrackerManager) StartVM(ctx context.Context, req *StartVMRequest) (int, error) {
	// --- Dry-run short-circuit ---
	// Must be first: no mkdir, no socket removal, no binary launch.
	// FIRECRACKER_DRY_RUN=true is the local-dev escape hatch for hosts
	// where /run is read-only or the firecracker binary is absent.
	if f.dryRun {
		f.log.Warn("FIRECRACKER_DRY_RUN=true: skipping VM launch — no runtime dirs, no binary exec",
			"instance_id", req.InstanceID,
			"synthetic_pid", dryRunPID,
		)
		return dryRunPID, nil
	}

	// --- Idempotency check ---
	if pid, err := f.readPID(req.InstanceID); err == nil && pid > 0 {
		if f.processAlive(pid) {
			f.log.Info("VM already running — idempotent no-op",
				"instance_id", req.InstanceID,
				"pid", pid,
			)
			return pid, nil
		}
		// Stale PID file (process died): clean up and continue.
		_ = os.Remove(f.pidFilePath(req.InstanceID))
	}

	kernelPath := req.KernelPath
	if kernelPath == "" {
		kernelPath = f.kernelPath
	}

	sockPath := f.socketPath(req.InstanceID)

	// --- Prepare directories ---
	for _, dir := range []string{f.socketDir, f.pidDir} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return 0, fmt.Errorf("StartVM: mkdir %s: %w", dir, err)
		}
	}

	// --- Remove stale socket ---
	_ = os.Remove(sockPath)

	// --- Launch firecracker process ---
	//nolint:gosec // instanceID is validated by the caller
	fcCmd := exec.CommandContext(ctx, "firecracker", "--api-sock", sockPath)
	fcCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // detach from parent process group
	if err := fcCmd.Start(); err != nil {
		return 0, fmt.Errorf("StartVM: launch firecracker: %w", err)
	}
	pid := fcCmd.Process.Pid

	// --- Wait for socket ---
	if err := f.waitForSocket(sockPath, socketReadyTimeout); err != nil {
		_ = fcCmd.Process.Kill()
		return 0, fmt.Errorf("StartVM: wait for socket: %w", err)
	}

	client := f.apiClient(sockPath)

	// --- Configure machine (vCPUs, memory) ---
	if err := f.putMachineConfig(client, req.CPUCores, req.MemoryMB); err != nil {
		_ = fcCmd.Process.Kill()
		return 0, fmt.Errorf("StartVM: machine config: %w", err)
	}

	// --- Set kernel boot source ---
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off nomodules ip=%s:::%s::eth0:off",
		req.PrivateIP, "255.255.255.0",
	)
	if err := f.putBootSource(client, kernelPath, bootArgs); err != nil {
		_ = fcCmd.Process.Kill()
		return 0, fmt.Errorf("StartVM: boot source: %w", err)
	}

	// --- Attach root drive ---
	if err := f.putDrive(client, req.RootfsPath); err != nil {
		_ = fcCmd.Process.Kill()
		return 0, fmt.Errorf("StartVM: drive: %w", err)
	}

	// --- Add network interface ---
	if err := f.putNetworkInterface(client, req.TapDevice, req.MacAddress); err != nil {
		_ = fcCmd.Process.Kill()
		return 0, fmt.Errorf("StartVM: network interface: %w", err)
	}

	// --- Issue InstanceStart action ---
	if err := f.startInstance(client); err != nil {
		_ = fcCmd.Process.Kill()
		return 0, fmt.Errorf("StartVM: start action: %w", err)
	}

	// --- Write PID file ---
	if err := f.writePID(req.InstanceID, pid); err != nil {
		// VM is running but we can't track it — kill and report error.
		_ = fcCmd.Process.Kill()
		return 0, fmt.Errorf("StartVM: write pid file: %w", err)
	}

	f.log.Info("VM started",
		"instance_id", req.InstanceID,
		"pid", pid,
		"vcpus", req.CPUCores,
		"memory_mb", req.MemoryMB,
		"private_ip", req.PrivateIP,
		"tap", req.TapDevice,
	)
	return pid, nil
}

// StopVM gracefully shuts down a running VM.
// Two-phase: sends ACPI power-off via Firecracker API, waits acpiGracePeriod,
// then force-kills the process with SIGKILL.
// Idempotent: if the process is not found, returns nil.
//
// Source: IMPLEMENTATION_PLAN_V1 §28 (stop_vm: ACPI + force fallback).
func (f *FirecrackerManager) StopVM(ctx context.Context, instanceID string) error {
	pid, err := f.readPID(instanceID)
	if err != nil || pid == 0 {
		f.log.Info("VM already absent — idempotent no-op", "instance_id", instanceID)
		return nil
	}
	if !f.processAlive(pid) {
		_ = os.Remove(f.pidFilePath(instanceID))
		f.log.Info("VM process already dead — idempotent no-op", "instance_id", instanceID, "pid", pid)
		return nil
	}

	sockPath := f.socketPath(instanceID)
	client := f.apiClient(sockPath)

	// --- Phase 1: ACPI power-off ---
	acpiErr := f.sendPowerOff(client)
	if acpiErr != nil {
		f.log.Warn("ACPI power-off failed — will force-kill",
			"instance_id", instanceID,
			"error", acpiErr,
		)
	} else {
		// Wait for graceful shutdown.
		deadline := time.Now().Add(acpiGracePeriod)
		for time.Now().Before(deadline) {
			if !f.processAlive(pid) {
				f.log.Info("VM stopped gracefully via ACPI",
					"instance_id", instanceID, "pid", pid)
				_ = f.cleanup(instanceID)
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
		f.log.Warn("ACPI grace period expired — force-killing",
			"instance_id", instanceID, "pid", pid)
	}

	// --- Phase 2: SIGKILL ---
	proc, err := os.FindProcess(pid)
	if err != nil {
		// Process already gone.
		_ = f.cleanup(instanceID)
		return nil
	}
	if err := proc.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
		return fmt.Errorf("StopVM: kill pid %d: %w", pid, err)
	}

	f.log.Info("VM force-killed", "instance_id", instanceID, "pid", pid)
	_ = f.cleanup(instanceID)
	return nil
}

// DeleteVM asserts that a VM is fully gone and cleans up all process artifacts.
// Called after StopVM as part of INSTANCE_DELETE.
// Idempotent: if nothing remains, returns nil.
//
// Source: IMPLEMENTATION_PLAN_V1 §29 (delete_vm_storage — process side).
func (f *FirecrackerManager) DeleteVM(instanceID string) error {
	pid, _ := f.readPID(instanceID)
	if pid > 0 && f.processAlive(pid) {
		// Caller should have called StopVM first — attempt kill as a safety net.
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Kill()
		}
		f.log.Warn("DeleteVM: VM was still running — killed",
			"instance_id", instanceID, "pid", pid)
	}
	return f.cleanup(instanceID)
}

// cleanup removes the pid file and socket for the instance.
func (f *FirecrackerManager) cleanup(instanceID string) error {
	var errs []string
	if err := os.Remove(f.pidFilePath(instanceID)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("pid file: %v", err))
	}
	if err := os.Remove(f.socketPath(instanceID)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("socket: %v", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("cleanup %s: %s", instanceID, strings.Join(errs, "; "))
	}
	return nil
}

// processAlive checks if a process with the given PID is alive.
func (f *FirecrackerManager) processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

func (f *FirecrackerManager) readPID(instanceID string) (int, error) {
	data, err := os.ReadFile(f.pidFilePath(instanceID))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func (f *FirecrackerManager) writePID(instanceID string, pid int) error {
	return os.WriteFile(f.pidFilePath(instanceID), []byte(strconv.Itoa(pid)), 0640)
}

// waitForSocket polls until the Unix socket file appears.
func (f *FirecrackerManager) waitForSocket(sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear within %s", sockPath, timeout)
}

// apiClient returns an http.Client configured to communicate over the Firecracker Unix socket.
func (f *FirecrackerManager) apiClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: fcAPITimeout,
	}
}

// Firecracker API helpers — all PUT to http://localhost/<resource>

func (f *FirecrackerManager) putMachineConfig(client *http.Client, cpuCores, memoryMB int32) error {
	body := map[string]any{
		"vcpu_count":   cpuCores,
		"mem_size_mib": memoryMB,
	}
	return f.fcPUT(client, "/machine-config", body)
}

func (f *FirecrackerManager) putBootSource(client *http.Client, kernelPath, bootArgs string) error {
	body := map[string]any{
		"kernel_image_path": kernelPath,
		"boot_args":         bootArgs,
	}
	return f.fcPUT(client, "/boot-source", body)
}

func (f *FirecrackerManager) putDrive(client *http.Client, rootfsPath string) error {
	body := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}
	return f.fcPUT(client, "/drives/rootfs", body)
}

func (f *FirecrackerManager) putNetworkInterface(client *http.Client, tapDevice, macAddr string) error {
	body := map[string]any{
		"iface_id":      "eth0",
		"host_dev_name": tapDevice,
		"guest_mac":     macAddr,
	}
	return f.fcPUT(client, "/network-interfaces/eth0", body)
}

func (f *FirecrackerManager) startInstance(client *http.Client) error {
	body := map[string]any{"action_type": "InstanceStart"}
	return f.fcPUT(client, "/actions", body)
}

func (f *FirecrackerManager) sendPowerOff(client *http.Client) error {
	body := map[string]any{"action_type": "SendCtrlAltDel"}
	return f.fcPUT(client, "/actions", body)
}

func (f *FirecrackerManager) fcPUT(client *http.Client, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPut, "http://localhost"+path,
		strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("build request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: HTTP %d", path, resp.StatusCode)
	}
	return nil
}

// listPIDFiles returns all instance IDs that have a pid file in the pid directory.
// Used by ListInstances to enumerate tracked VMs.
func (f *FirecrackerManager) listPIDFiles() ([]string, error) {
	entries, err := os.ReadDir(f.pidDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".pid") {
			ids = append(ids, strings.TrimSuffix(name, ".pid"))
		}
	}
	return ids, nil
}

// ── VMRuntime interface implementation ────────────────────────────────────────

// Ensure FirecrackerManager implements VMRuntime.
var _ VMRuntime = (*FirecrackerManager)(nil)

// Create implements VMRuntime.Create by delegating to StartVM.
// Maps InstanceSpec → StartVMRequest and returns RuntimeInfo.
func (f *FirecrackerManager) Create(ctx context.Context, spec InstanceSpec) (*RuntimeInfo, error) {
	req := &StartVMRequest{
		InstanceID:   spec.InstanceID,
		KernelPath:   spec.KernelPath,
		RootfsPath:   spec.RootfsPath,
		CPUCores:     spec.CPUCores,
		MemoryMB:     spec.MemoryMB,
		TapDevice:    spec.TapDevice,
		MacAddress:   spec.MacAddress,
		PrivateIP:    spec.PrivateIP,
		SSHPublicKey: spec.SSHPublicKey,
	}
	if req.KernelPath == "" {
		req.KernelPath = f.kernelPath
	}
	pid, err := f.StartVM(ctx, req)
	if err != nil {
		return nil, err
	}
	return &RuntimeInfo{
		InstanceID: spec.InstanceID,
		State:      "RUNNING",
		PID:        int32(pid),
		DataDir:    f.pidDir,
	}, nil
}

// Start implements VMRuntime.Start. Phase 1: not implemented; use Create.
func (f *FirecrackerManager) Start(ctx context.Context, instanceID string) error {
	return fmt.Errorf("FirecrackerManager.Start: Phase 1 — not implemented; use Create for full re-provision")
}

// Stop implements VMRuntime.Stop.
func (f *FirecrackerManager) Stop(ctx context.Context, instanceID string) error {
	return f.StopVM(ctx, instanceID)
}

// Reboot implements VMRuntime.Reboot. Phase 1: not implemented.
func (f *FirecrackerManager) Reboot(ctx context.Context, instanceID string) error {
	return fmt.Errorf("FirecrackerManager.Reboot: Phase 1 — not implemented; use Stop + Create")
}

// Delete implements VMRuntime.Delete.
func (f *FirecrackerManager) Delete(ctx context.Context, instanceID string) error {
	return f.DeleteVM(instanceID)
}

// Inspect implements VMRuntime.Inspect.
func (f *FirecrackerManager) Inspect(ctx context.Context, instanceID string) (*RuntimeInfo, error) {
	pid, err := f.readPID(instanceID)
	if err != nil {
		return nil, fmt.Errorf("FirecrackerManager.Inspect: %s: %w", instanceID, err)
	}
	state := "RUNNING"
	if !f.processAlive(pid) {
		state = "STOPPED"
	}
	return &RuntimeInfo{
		InstanceID: instanceID,
		State:      state,
		PID:        int32(pid),
		DataDir:    f.pidDir,
	}, nil
}

// List implements VMRuntime.List.
func (f *FirecrackerManager) List(ctx context.Context) ([]RuntimeInfo, error) {
	ids, err := f.listPIDFiles()
	if err != nil {
		return nil, err
	}
	var result []RuntimeInfo
	for _, id := range ids {
		pid, readErr := f.readPID(id)
		state := "STOPPED"
		if readErr == nil && pid > 0 && f.processAlive(pid) {
			state = "RUNNING"
		}
		result = append(result, RuntimeInfo{
			InstanceID: id,
			State:      state,
			PID:        int32(pid),
			DataDir:    f.pidDir,
		})
	}
	return result, nil
}

// DataRoot returns the configured runtime data root directory (the PID dir).
func (f *FirecrackerManager) DataRoot() string { return f.pidDir }
