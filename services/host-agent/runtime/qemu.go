package runtime

// qemu.go — QEMU/KVM VM launch and lifecycle management.
//
// Source: VM Job 2 — QEMU/KVM as first concrete runtime backend.
//
// Implements VMRuntime for QEMU/KVM. Uses QEMU command-line invocation
// with QMP (QEMU Machine Protocol) for runtime control over a Unix socket.
//
// VM lifecycle:
//   Create  — launch qemu-system-x86_64 with configured spec, capture console
//   Start   — send QMP "cont" command (resume from stopped state)
//   Stop    — send QMP "system_powerdown" (graceful), then SIGKILL on timeout
//   Reboot  — send QMP "system_reset"
//   Delete  — kill process, remove PID file, socket, console, instance dir
//   Inspect — check PID liveness, read state from QMP or PID file
//   List    — scan instance directories under data root
//
// Idempotent: all operations check current state before acting.
//
// Required on host:
//   - qemu-system-x86_64 on PATH
//   - KVM device (/dev/kvm) accessible (optional; falls back to TCG emulation)
//
// Artifact layout (per instance):
//   <dataRoot>/<instanceID>/
//     instance.pid     — PID file
//     instance.sock    — QMP control socket
//     console.log      — serial console output

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Ensure QemuManager implements VMRuntime.
var _ VMRuntime = (*QemuManager)(nil)

const (
	qmpSocketReadyTimeout = 10 * time.Second
	qemuGracePeriod       = 30 * time.Second
	qmpTimeout            = 5 * time.Second
)

// QemuManager implements VMRuntime using QEMU/KVM.
type QemuManager struct {
	artifacts   *ArtifactManager
	console     *ConsoleLogger
	configDrive *ConfigDriveManager
	dryRun      bool // QEMU_DRY_RUN=true: skip binary launch
	log         *slog.Logger
}

// NewQemuManager constructs a QemuManager.
// dataRoot: top-level directory for instance artifacts.
func NewQemuManager(dataRoot string, log *slog.Logger) *QemuManager {
	artifacts := NewArtifactManager(dataRoot)
	if log == nil {
		log = slog.Default()
	}
	return &QemuManager{
		artifacts:   artifacts,
		console:     NewConsoleLogger(artifacts),
		configDrive: NewConfigDriveManager(artifacts),
		dryRun:      os.Getenv("QEMU_DRY_RUN") == "true",
		log:         log,
	}
}

func (q *QemuManager) DataRoot() string { return q.artifacts.DataRoot() }

// ── VMRuntime implementation ──────────────────────────────────────────────────

func (q *QemuManager) Create(ctx context.Context, spec InstanceSpec) (*RuntimeInfo, error) {
	// --- Dry-run short-circuit ---
	if q.dryRun {
		q.log.Warn("QEMU_DRY_RUN=true: skipping VM launch", "instance_id", spec.InstanceID)
		return &RuntimeInfo{
			InstanceID: spec.InstanceID,
			State:      "RUNNING",
			PID:        dryRunPID,
			DataDir:    q.artifacts.InstanceDir(spec.InstanceID),
		}, nil
	}

	// --- Idempotency check ---
	if info, err := q.Inspect(ctx, spec.InstanceID); err == nil && info.State == "RUNNING" {
		q.log.Info("VM already running — idempotent no-op", "instance_id", spec.InstanceID, "pid", info.PID)
		return info, nil
	}

	// Ensure instance directory.
	if err := q.artifacts.EnsureInstanceDir(spec.InstanceID); err != nil {
		return nil, fmt.Errorf("QemuManager.Create: %w", err)
	}

	// Generate cloud-init config-drive seed ISO (best-effort).
	if spec.SSHPublicKey != "" {
		seedPath, seedErr := q.configDrive.GenerateSeed(spec.InstanceID, CloudInitConfig{
			InstanceID:   spec.InstanceID,
			Hostname:     spec.InstanceID,
			SSHPublicKey: spec.SSHPublicKey,
		})
		if seedErr != nil {
			q.log.Warn("cloud-init seed generation failed — VM will boot without config-drive",
				"instance_id", spec.InstanceID, "error", seedErr)
		} else {
			q.log.Info("cloud-init seed ISO generated", "instance_id", spec.InstanceID, "path", seedPath)
		}
	}

	// Build QEMU command.
	args, err := q.buildQEMUArgs(spec)
	if err != nil {
		return nil, fmt.Errorf("QemuManager.Create: build args: %w", err)
	}

	// Remove stale socket if present.
	_ = os.Remove(q.artifacts.SocketPath(spec.InstanceID))

	// Launch QEMU process.
	q.log.Info("launching QEMU", "instance_id", spec.InstanceID, "vcpus", spec.CPUCores, "mem_mb", spec.MemoryMB)

	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("QemuManager.Create: launch qemu: %w", err)
	}

	pid := cmd.Process.Pid

	// Wait for QMP socket to appear.
	if err := q.waitForSocket(q.artifacts.SocketPath(spec.InstanceID), qmpSocketReadyTimeout); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("QemuManager.Create: wait for QMP socket: %w", err)
	}

	// Write PID file.
	if err := q.writePID(spec.InstanceID, pid); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("QemuManager.Create: write pid: %w", err)
	}

	q.log.Info("QEMU VM started", "instance_id", spec.InstanceID, "pid", pid)
	return &RuntimeInfo{
		InstanceID: spec.InstanceID,
		State:      "RUNNING",
		PID:        int32(pid),
		DataDir:    q.artifacts.InstanceDir(spec.InstanceID),
	}, nil
}

func (q *QemuManager) Start(ctx context.Context, instanceID string) error {
	// Phase 1: Start == Create (full re-provision). Workers call Create directly.
	// The QMP "cont" command is for paused VMs (not implemented in Phase 1).
	return fmt.Errorf("QemuManager.Start: Phase 1 — not implemented; use Create")
}

func (q *QemuManager) Stop(ctx context.Context, instanceID string) error {
	pid, err := q.readPID(instanceID)
	if err != nil {
		q.log.Info("VM already absent — idempotent no-op", "instance_id", instanceID)
		return nil
	}
	if !processAliveOS(pid) {
		_ = os.Remove(q.artifacts.PIDPath(instanceID))
		q.log.Info("VM process already dead — idempotent no-op", "instance_id", instanceID, "pid", pid)
		return nil
	}

	// Try QMP system_powerdown.
	sockPath := q.artifacts.SocketPath(instanceID)
	if _, statErr := os.Stat(sockPath); statErr == nil {
		if qmpErr := q.sendQMPCommand(sockPath, "system_powerdown", nil); qmpErr != nil {
			q.log.Warn("QMP system_powerdown failed — will force-kill", "instance_id", instanceID, "error", qmpErr)
		} else {
			// Wait for graceful shutdown.
			deadline := time.Now().Add(qemuGracePeriod)
			for time.Now().Before(deadline) {
				if !processAliveOS(pid) {
					q.log.Info("VM stopped gracefully", "instance_id", instanceID, "pid", pid)
					_ = q.cleanupArtifacts(instanceID)
					return nil
				}
				time.Sleep(500 * time.Millisecond)
			}
			q.log.Warn("QEMU grace period expired — force-killing", "instance_id", instanceID, "pid", pid)
		}
	}

	// Force kill.
	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = q.cleanupArtifacts(instanceID)
		return nil
	}
	_ = proc.Kill()
	q.log.Info("VM force-killed", "instance_id", instanceID, "pid", pid)
	_ = q.cleanupArtifacts(instanceID)
	return nil
}

func (q *QemuManager) Reboot(ctx context.Context, instanceID string) error {
	sockPath := q.artifacts.SocketPath(instanceID)
	if _, err := os.Stat(sockPath); err != nil {
		return fmt.Errorf("QemuManager.Reboot: socket not found for %s", instanceID)
	}
	if err := q.sendQMPCommand(sockPath, "system_reset", nil); err != nil {
		return fmt.Errorf("QemuManager.Reboot: %w", err)
	}
	q.log.Info("VM reboot issued", "instance_id", instanceID)
	return nil
}

func (q *QemuManager) Delete(ctx context.Context, instanceID string) error {
	pid, _ := q.readPID(instanceID)
	if pid > 0 && processAliveOS(pid) {
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Kill()
		}
		q.log.Warn("QemuManager.Delete: VM was still running — killed", "instance_id", instanceID, "pid", pid)
	}
	_ = q.cleanupArtifacts(instanceID)
	return q.artifacts.RemoveInstanceDir(instanceID)
}

func (q *QemuManager) Inspect(ctx context.Context, instanceID string) (*RuntimeInfo, error) {
	pid, err := q.readPID(instanceID)
	if err != nil {
		return nil, fmt.Errorf("QemuManager.Inspect: %s: %w", instanceID, err)
	}
	state := "RUNNING"
	if !processAliveOS(pid) {
		state = "STOPPED"
	}

	return &RuntimeInfo{
		InstanceID: instanceID,
		State:      state,
		PID:        int32(pid),
		DataDir:    q.artifacts.InstanceDir(instanceID),
	}, nil
}

func (q *QemuManager) List(ctx context.Context) ([]RuntimeInfo, error) {
	ids, err := q.artifacts.InstanceIDs()
	if err != nil {
		return nil, err
	}
	var result []RuntimeInfo
	for _, id := range ids {
		info, inspectErr := q.Inspect(ctx, id)
		if inspectErr != nil {
			info = &RuntimeInfo{InstanceID: id, State: "DELETED", PID: 0, DataDir: q.artifacts.InstanceDir(id)}
		}
		result = append(result, *info)
	}
	return result, nil
}

// ── QEMU command generation ──────────────────────────────────────────────────

// buildQEMUArgs constructs the QEMU command-line arguments for a VM spec.
// Exported for testability.
func (q *QemuManager) buildQEMUArgs(spec InstanceSpec) ([]string, error) {
	sockPath := q.artifacts.SocketPath(spec.InstanceID)
	consolePath := q.artifacts.ConsolePath(spec.InstanceID)

	// Ensure console file exists for QEMU to write to.
	if err := q.console.EnsureConsoleFile(spec.InstanceID); err != nil {
		return nil, fmt.Errorf("console file: %w", err)
	}

	args := []string{
		"-name", spec.InstanceID,
		"-machine", "q35,accel=kvm:tcg",
		"-cpu", "host",
		"-smp", fmt.Sprintf("cpus=%d", spec.CPUCores),
		"-m", fmt.Sprintf("%dM", spec.MemoryMB),
		"-drive", fmt.Sprintf("file=%s,if=virtio,format=qcow2", spec.RootfsPath),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", spec.TapDevice),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", spec.MacAddress),
		"-serial", "file:" + consolePath,
		"-qmp", "unix:" + sockPath + ",server,nowait",
		"-display", "none",
		"-daemonize",
		"-pidfile", q.artifacts.PIDPath(spec.InstanceID),
		// Guest agent for graceful shutdown:
		"-chardev", "socket,path=" + q.artifacts.SocketPath(spec.InstanceID) + ".qga,server=on,wait=off,id=qga0",
		"-device", "virtio-serial",
		"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
	}

	if spec.KernelPath != "" {
		args = append(args, "-kernel", spec.KernelPath)
		args = append(args, "-append", fmt.Sprintf("console=ttyS0 ip=%s:::255.255.255.0::eth0:off", spec.PrivateIP))
	}

	// Cloud-init config-drive seed ISO if present.
	seedPath := q.configDrive.SeedPath(spec.InstanceID)
	if _, err := os.Stat(seedPath); err == nil {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,if=virtio,media=cdrom", seedPath),
		)
	}

	// Extra block devices from attached volumes.
	// Each volume maps to a virtio-blk drive with a deterministic serial.
	for _, d := range spec.ExtraDisks {
		if d.HostPath == "" {
			continue
		}
		serial := "vol-" + d.DiskID
		if len(serial) > 31 {
			serial = serial[:31]
		}
		args = append(args,
			"-drive",
			fmt.Sprintf("file=%s,if=virtio,format=qcow2,serial=%s", d.HostPath, serial),
		)
	}

	return args, nil
}

// ── QMP communication ────────────────────────────────────────────────────────

// qmpCommand is a JSON command sent to the QMP socket.
type qmpCommand struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// qmpResponse is a JSON response from QMP.
type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

func (q *QemuManager) sendQMPCommand(sockPath, cmd string, args map[string]any) error {
	conn, err := net.DialTimeout("unix", sockPath, qmpTimeout)
	if err != nil {
		return fmt.Errorf("QMP dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(qmpTimeout))

	// Read QMP greeting (QEMU sends a greeting on connect).
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("QMP read greeting: %w", err)
	}
	var greeting qmpResponse
	if err := json.Unmarshal([]byte(line), &greeting); err != nil {
		return fmt.Errorf("QMP parse greeting: %w", err)
	}
	// The greeting contains QMP capabilities — acknowledge them.
	qmpCaps := qmpCommand{Execute: "qmp_capabilities"}
	if err := q.writeQMP(conn, qmpCaps); err != nil {
		return fmt.Errorf("QMP capabilities: %w", err)
	}
	if _, err := q.readQMPResponse(reader); err != nil {
		return fmt.Errorf("QMP caps response: %w", err)
	}

	// Send the actual command.
	qCmd := qmpCommand{Execute: cmd, Arguments: args}
	if err := q.writeQMP(conn, qCmd); err != nil {
		return fmt.Errorf("QMP %s: %w", cmd, err)
	}
	if _, err := q.readQMPResponse(reader); err != nil {
		return fmt.Errorf("QMP %s response: %w", cmd, err)
	}
	return nil
}

func (q *QemuManager) writeQMP(conn net.Conn, cmd qmpCommand) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}

func (q *QemuManager) readQMPResponse(r *bufio.Reader) (*qmpResponse, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var resp qmpResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("parse QMP response: %w (raw: %s)", err, strings.TrimSpace(line))
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("QMP error: %s: %s", resp.Error.Class, resp.Error.Desc)
	}
	return &resp, nil
}

// ── PID management ───────────────────────────────────────────────────────────

func (q *QemuManager) pidPath(instanceID string) string {
	return q.artifacts.PIDPath(instanceID)
}

func (q *QemuManager) readPID(instanceID string) (int, error) {
	data, err := os.ReadFile(q.pidPath(instanceID))
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse PID: %w", err)
	}
	return v, nil
}

func (q *QemuManager) writePID(instanceID string, pid int) error {
	return os.WriteFile(q.pidPath(instanceID), []byte(strconv.Itoa(pid)), 0640)
}

func (q *QemuManager) cleanupArtifacts(instanceID string) error {
	var errs []string
	for _, p := range []string{q.artifacts.PIDPath(instanceID), q.artifacts.SocketPath(instanceID)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("%s: %v", filepath.Base(p), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("cleanup %s: %s", instanceID, strings.Join(errs, "; "))
	}
	return nil
}

func (q *QemuManager) waitForSocket(sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear within %s", sockPath, timeout)
}

// processAliveOS checks if a process with the given PID is alive.
func processAliveOS(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
