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
	}
	// Copy and modify
	copy := info
	copy.State = "STOPPED"
	if info.State != "RUNNING" {
		t.Error("modifying copy should not affect original")
	}
}
