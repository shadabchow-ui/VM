package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// TestVMRuntime_InterfaceConformance verifies all implementations satisfy the interface.
func TestVMRuntime_InterfaceConformance(t *testing.T) {
	var _ VMRuntime = (*FirecrackerManager)(nil)
	var _ VMRuntime = (*QemuManager)(nil)
	var _ VMRuntime = (*FakeRuntime)(nil)
}

// TestFakeRuntime_Create verifies Create records the call and stores state.
func TestFakeRuntime_Create(t *testing.T) {
	fr := NewFakeRuntime()

	spec := InstanceSpec{
		InstanceID: "inst-001",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/tmp/test.qcow2",
		TapDevice:  "tap-test",
	}

	info, err := fr.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.InstanceID != "inst-001" {
		t.Errorf("InstanceID = %q", info.InstanceID)
	}
	if info.State != "RUNNING" {
		t.Errorf("State = %q, want RUNNING", info.State)
	}
	if info.PID == 0 {
		t.Error("PID should not be 0")
	}
	if fr.CallCount() != 1 {
		t.Errorf("CallCount = %d, want 1", fr.CallCount())
	}

	last := fr.LastCall()
	if last.Op != "Create" || last.InstanceID != "inst-001" {
		t.Errorf("LastCall = %+v", last)
	}
}

// TestFakeRuntime_FullLifecycle tests create → inspect → stop → inspect → delete.
func TestFakeRuntime_FullLifecycle(t *testing.T) {
	fr := NewFakeRuntime()
	ctx := t.Context()

	// Create
	spec := InstanceSpec{InstanceID: "inst-lifecycle", CPUCores: 2, MemoryMB: 4096, RootfsPath: "/tmp/test.qcow2"}
	info, err := fr.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.State != "RUNNING" {
		t.Errorf("after create: State = %q, want RUNNING", info.State)
	}

	// Inspect (should find it)
	info, err = fr.Inspect(ctx, "inst-lifecycle")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.State != "RUNNING" {
		t.Errorf("after inspect: State = %q, want RUNNING", info.State)
	}

	// Stop
	if err := fr.Stop(ctx, "inst-lifecycle"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Inspect (should be STOPPED)
	info, err = fr.Inspect(ctx, "inst-lifecycle")
	if err != nil {
		t.Fatalf("Inspect after stop: %v", err)
	}
	if info.State != "STOPPED" {
		t.Errorf("after stop: State = %q, want STOPPED", info.State)
	}
	if info.PID != 0 {
		t.Errorf("after stop: PID = %d, want 0", info.PID)
	}

	// Delete
	if err := fr.Delete(ctx, "inst-lifecycle"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Inspect (should be gone)
	_, err = fr.Inspect(ctx, "inst-lifecycle")
	if err == nil {
		t.Error("expected error on Inspect after Delete, got nil")
	}
}

// TestFakeRuntime_ListAllInstances verifies List returns created instances.
func TestFakeRuntime_ListAllInstances(t *testing.T) {
	fr := NewFakeRuntime()
	ctx := t.Context()

	for i := 1; i <= 3; i++ {
		spec := InstanceSpec{InstanceID: fmt.Sprintf("inst-%03d", i), CPUCores: 1, MemoryMB: 1024, RootfsPath: "/tmp/test.qcow2"}
		if _, err := fr.Create(ctx, spec); err != nil {
			t.Fatalf("Create inst-%03d: %v", i, err)
		}
	}

	infos, err := fr.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 3 {
		t.Errorf("List count = %d, want 3", len(infos))
	}
}

// TestFakeRuntime_IdempotentStop verifies Stop on already-stopped instance is a no-op.
func TestFakeRuntime_IdempotentStop(t *testing.T) {
	fr := NewFakeRuntime()
	ctx := t.Context()

	spec := InstanceSpec{InstanceID: "inst-idem", CPUCores: 2, MemoryMB: 4096, RootfsPath: "/tmp/test.qcow2"}
	if _, err := fr.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First stop succeeds.
	if err := fr.Stop(ctx, "inst-idem"); err != nil {
		t.Fatalf("first Stop: %v", err)
	}

	// Second stop succeeds (idempotent).
	if err := fr.Stop(ctx, "inst-idem"); err != nil {
		t.Fatalf("second Stop (idempotent): %v", err)
	}
}

// TestFakeRuntime_IdempotentDelete verifies Delete on already-deleted instance is a no-op.
func TestFakeRuntime_IdempotentDelete(t *testing.T) {
	fr := NewFakeRuntime()
	ctx := t.Context()

	spec := InstanceSpec{InstanceID: "inst-idem", CPUCores: 2, MemoryMB: 4096, RootfsPath: "/tmp/test.qcow2"}
	if _, err := fr.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First delete succeeds.
	if err := fr.Delete(ctx, "inst-idem"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}

	// Second delete succeeds (idempotent).
	if err := fr.Delete(ctx, "inst-idem"); err != nil {
		t.Fatalf("second Delete (idempotent): %v", err)
	}
}

// TestFakeRuntime_CreateError verifies error injection.
func TestFakeRuntime_CreateError(t *testing.T) {
	fr := NewFakeRuntime()
	fr.Errors["Create:inst-fail"] = errors.New("injected create error")

	_, err := fr.Create(t.Context(), InstanceSpec{InstanceID: "inst-fail", CPUCores: 1, MemoryMB: 1024})
	if err == nil {
		t.Fatal("expected injected error, got nil")
	}
	if err.Error() != "injected create error" {
		t.Errorf("error = %q, want 'injected create error'", err.Error())
	}
}

// TestFakeRuntime_StopError verifies error injection on Stop.
func TestFakeRuntime_StopError(t *testing.T) {
	fr := NewFakeRuntime()
	fr.Errors["Stop:*"] = errors.New("all stops fail")

	err := fr.Stop(t.Context(), "any-instance")
	if err == nil {
		t.Fatal("expected injected error, got nil")
	}
}

// TestFakeRuntime_Reset clears state.
func TestFakeRuntime_Reset(t *testing.T) {
	fr := NewFakeRuntime()
	ctx := t.Context()

	spec := InstanceSpec{InstanceID: "inst-reset", CPUCores: 1, MemoryMB: 1024, RootfsPath: "/tmp/test.qcow2"}
	if _, err := fr.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fr.Reset()

	if fr.CallCount() != 0 {
		t.Errorf("CallCount after reset = %d, want 0", fr.CallCount())
	}
	if len(fr.KnownInstances) != 0 {
		t.Errorf("KnownInstances after reset = %d, want 0", len(fr.KnownInstances))
	}
}

// TestFakeRuntime_DataRoot returns the configured data root.
func TestFakeRuntime_DataRoot(t *testing.T) {
	fr := NewFakeRuntime()
	if fr.DataRoot() != "/tmp/vm-platform-fake" {
		t.Errorf("DataRoot = %q", fr.DataRoot())
	}
}

// TestFakeRuntime_Reboot records the call.
func TestFakeRuntime_Reboot(t *testing.T) {
	fr := NewFakeRuntime()
	if err := fr.Reboot(t.Context(), "inst-001"); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if fr.CallCount() != 1 {
		t.Errorf("CallCount = %d, want 1", fr.CallCount())
	}
}

// TestFakeRuntime_Start records the call.
func TestFakeRuntime_Start(t *testing.T) {
	fr := NewFakeRuntime()
	if err := fr.Start(t.Context(), "inst-001"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fr.CallCount() != 1 {
		t.Errorf("CallCount = %d, want 1", fr.CallCount())
	}
}

// TestConfig_Defaults verifies runtime config defaults.
func TestConfig_Defaults(t *testing.T) {
	cfg := DefaultRuntimeConfig()
	if cfg.DataRoot != DefaultDataRoot {
		t.Errorf("DataRoot = %q, want %q", cfg.DataRoot, DefaultDataRoot)
	}
	if cfg.Backend != RuntimeFirecracker {
		t.Errorf("Backend = %q, want %q", cfg.Backend, RuntimeFirecracker)
	}
}

// TestConfig_BackendEnvVar verifies backend selection via env var.
func TestConfig_BackendEnvVar(t *testing.T) {
	t.Setenv("VM_PLATFORM_RUNTIME", "qemu")
	cfg := DefaultRuntimeConfig()
	if cfg.Backend != "qemu" {
		t.Errorf("Backend = %q, want qemu", cfg.Backend)
	}
}

// TestRuntimeInfo_Fields checks RuntimeInfo is a value type safe for copies.
func TestRuntimeInfo_Fields(t *testing.T) {
	info := RuntimeInfo{
		InstanceID: "inst-test",
		State:      "RUNNING",
		PID:        12345,
		DataDir:    "/var/lib/compute-platform/instances/inst-test",
		HostID:     "host-001",
		TapDevice:  "tap-inst-test",
		DiskPaths:  []string{"/mnt/nfs/vols/inst-test.qcow2"},
		SocketPath: "/var/lib/compute-platform/instances/inst-test/firecracker.sock",
		LogPath:    "/var/lib/compute-platform/instances/inst-test/console.log",
		CPUCores:   2,
		MemoryMB:   4096,
	}
	// Copy and modify
	copy := info
	copy.State = "STOPPED"
	if info.State != "RUNNING" {
		t.Error("modifying copy should not affect original")
	}
	if info.HostID != "host-001" {
		t.Error("HostID should persist")
	}
}

// TestFakeRuntime_InventoryShape verifies Create populates all RuntimeInfo fields
// needed by the reconciler for runtime-aware drift detection.
func TestFakeRuntime_InventoryShape(t *testing.T) {
	fr := NewFakeRuntime()
	fr.HostID = "host-test-001"

	spec := InstanceSpec{
		InstanceID: "inst-inv-001",
		CPUCores:   4,
		MemoryMB:   8192,
		RootfsPath: "/mnt/data/inst-inv-001/overlay.qcow2",
		TapDevice:  "tap-inst-inv-001",
		MacAddress: "aa:bb:cc:dd:ee:01",
		PrivateIP:  "10.0.0.5",
	}

	info, err := fr.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if info.InstanceID != "inst-inv-001" {
		t.Errorf("InstanceID = %q", info.InstanceID)
	}
	if info.State != "RUNNING" {
		t.Errorf("State = %q, want RUNNING", info.State)
	}
	if info.PID <= 0 {
		t.Errorf("PID = %d, want > 0", info.PID)
	}
	if info.HostID != "host-test-001" {
		t.Errorf("HostID = %q, want host-test-001", info.HostID)
	}
	if info.TapDevice != "tap-inst-inv-001" {
		t.Errorf("TapDevice = %q, want tap-inst-inv-001", info.TapDevice)
	}
	if info.SocketPath == "" {
		t.Error("SocketPath should not be empty")
	}
	if info.LogPath == "" {
		t.Error("LogPath should not be empty")
	}
	if len(info.DiskPaths) == 0 {
		t.Error("DiskPaths should not be empty")
	}
	if info.CPUCores != 4 {
		t.Errorf("CPUCores = %d, want 4", info.CPUCores)
	}
	if info.MemoryMB != 8192 {
		t.Errorf("MemoryMB = %d, want 8192", info.MemoryMB)
	}
}

// TestFakeRuntime_ListRichInventory verifies List returns instances with all
// inventory fields populated.
func TestFakeRuntime_ListRichInventory(t *testing.T) {
	fr := NewFakeRuntime()
	fr.HostID = "host-rich-001"
	ctx := t.Context()

	for i := 1; i <= 2; i++ {
		spec := InstanceSpec{
			InstanceID: fmt.Sprintf("inst-r-%03d", i),
			CPUCores:   int32(i * 2),
			MemoryMB:   int32(i * 4096),
			RootfsPath: fmt.Sprintf("/mnt/data/inst-r-%03d/overlay.qcow2", i),
			TapDevice:  fmt.Sprintf("tap-inst-r-%03d", i),
		}
		if _, err := fr.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	infos, err := fr.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List count = %d, want 2", len(infos))
	}

	for _, info := range infos {
		if info.HostID != "host-rich-001" {
			t.Errorf("%s: HostID = %q, want host-rich-001", info.InstanceID, info.HostID)
		}
		if info.TapDevice == "" {
			t.Errorf("%s: TapDevice is empty", info.InstanceID)
		}
		if info.SocketPath == "" {
			t.Errorf("%s: SocketPath is empty", info.InstanceID)
		}
		if info.LogPath == "" {
			t.Errorf("%s: LogPath is empty", info.InstanceID)
		}
		if info.CPUCores <= 0 {
			t.Errorf("%s: CPUCores = %d, want > 0", info.InstanceID, info.CPUCores)
		}
	}
}

// TestRuntimeInfo_IsRunning verifies the IsRunning helper.
func TestRuntimeInfo_IsRunning(t *testing.T) {
	r1 := RuntimeInfo{State: "RUNNING", PID: 1234}
	if !r1.IsRunning() {
		t.Error("RUNNING+PID>0: expected IsRunning=true")
	}
	r2 := RuntimeInfo{State: "RUNNING", PID: 0}
	if r2.IsRunning() {
		t.Error("RUNNING+PID=0: expected IsRunning=false")
	}
	r3 := RuntimeInfo{State: "STOPPED", PID: 1234}
	if r3.IsRunning() {
		t.Error("STOPPED+PID>0: expected IsRunning=false")
	}
	r4 := RuntimeInfo{State: "", PID: 0}
	if r4.IsRunning() {
		t.Error("empty+PID=0: expected IsRunning=false")
	}
}

// TestRuntimeInfo_IsPresent verifies the IsPresent helper.
func TestRuntimeInfo_IsPresent(t *testing.T) {
	p1 := RuntimeInfo{State: "RUNNING"}
	if !p1.IsPresent() {
		t.Error("RUNNING: expected IsPresent=true")
	}
	p2 := RuntimeInfo{State: "STOPPED"}
	if !p2.IsPresent() {
		t.Error("STOPPED: expected IsPresent=true")
	}
	p3 := RuntimeInfo{State: "DELETED"}
	if p3.IsPresent() {
		t.Error("DELETED: expected IsPresent=false")
	}
	p4 := RuntimeInfo{State: ""}
	if p4.IsPresent() {
		t.Error("empty: expected IsPresent=false")
	}
}
