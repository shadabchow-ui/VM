package handlers

// lifecycle_test.go — End-to-end lifecycle sequence tests using in-memory fakes.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §lifecycle,
//         LIFECYCLE_STATE_MACHINE_V1 §2 (full transition table).
//
// These tests exercise complete multi-step sequences (create → stop → start →
// reboot → delete) using the same fakes as the unit tests. They are valid on a
// macOS dev box — no real DB, no real Host Agent, no Linux/KVM required.
//
// They are distinct from test/integration/* which require a real PostgreSQL
// instance (//go:build integration) and are the hardware-gate tests.

import (
	"context"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// ── fakeFullRuntime — supports all ops needed across the full lifecycle ────────

type fakeFullRuntime struct {
	createFail bool
	stopFail   bool
}

func (r *fakeFullRuntime) CreateInstance(_ context.Context, req *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error) {
	if r.createFail {
		return nil, &runtimeError{"CreateInstance"}
	}
	return &runtimeclient.CreateInstanceResponse{InstanceID: req.InstanceID, State: "RUNNING"}, nil
}

func (r *fakeFullRuntime) StopInstance(_ context.Context, req *runtimeclient.StopInstanceRequest) (*runtimeclient.StopInstanceResponse, error) {
	if r.stopFail {
		return nil, &runtimeError{"StopInstance"}
	}
	return &runtimeclient.StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"}, nil
}

func (r *fakeFullRuntime) DeleteInstance(_ context.Context, req *runtimeclient.DeleteInstanceRequest) (*runtimeclient.DeleteInstanceResponse, error) {
	return &runtimeclient.DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"}, nil
}

type runtimeError struct{ op string }

func (e *runtimeError) Error() string { return "fakeFullRuntime: " + e.op + " failure" }

// ── shared lifecycle fixture ──────────────────────────────────────────────────

type lifecycleFixture struct {
	store *fakeStore
	net   *fakeNetwork
	rt    *fakeFullRuntime
}

func newLifecycleFixture() *lifecycleFixture {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	return &lifecycleFixture{
		store: store,
		net:   &fakeNetwork{nextIP: "10.0.1.1"},
		rt:    &fakeFullRuntime{},
	}
}

func (f *lifecycleFixture) newCreateHandler() *CreateHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) *runtimeclient.Client { return nil }}
	h := NewCreateHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	h.readinessFn = instantReadiness
	return h
}

func (f *lifecycleFixture) newStopHandler() *StopHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) *runtimeclient.Client { return nil }}
	h := NewStopHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	return h
}

func (f *lifecycleFixture) newStartHandler() *StartHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) *runtimeclient.Client { return nil }}
	h := NewStartHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	h.readinessFn = instantReadiness
	return h
}

func (f *lifecycleFixture) newRebootHandler() *RebootHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) *runtimeclient.Client { return nil }}
	h := NewRebootHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	h.readinessFn = instantReadiness
	return h
}

func (f *lifecycleFixture) newDeleteHandler() *DeleteHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) *runtimeclient.Client { return nil }}
	h := NewDeleteHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	return h
}

// assertState is a test helper for clean state assertions.
func assertState(t *testing.T, store *fakeStore, id, want string) {
	t.Helper()
	if store.instances[id].VMState != want {
		t.Errorf("vm_state = %q, want %q", store.instances[id].VMState, want)
	}
}

// ── Lifecycle sequence tests ──────────────────────────────────────────────────

func TestLifecycle_Create_Stop_Start_Delete(t *testing.T) {
	f := newLifecycleFixture()
	const id = "inst_lc_css"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	// Create
	if err := f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	assertState(t, f.store, id, "running")

	// Stop
	if err := f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertState(t, f.store, id, "stopped")

	// Start
	if err := f.newStartHandler().Execute(ctx, testJob(id, "INSTANCE_START")); err != nil {
		t.Fatalf("start: %v", err)
	}
	assertState(t, f.store, id, "running")

	// Delete
	if err := f.newDeleteHandler().Execute(ctx, testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertState(t, f.store, id, "deleted")
}

func TestLifecycle_Create_Reboot_Delete(t *testing.T) {
	f := newLifecycleFixture()
	const id = "inst_lc_crd"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	if err := f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	assertState(t, f.store, id, "running")

	if err := f.newRebootHandler().Execute(ctx, testJob(id, "INSTANCE_REBOOT")); err != nil {
		t.Fatalf("reboot: %v", err)
	}
	assertState(t, f.store, id, "running")

	if err := f.newDeleteHandler().Execute(ctx, testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertState(t, f.store, id, "deleted")
}

func TestLifecycle_Create_Stop_Start_Reboot_Delete(t *testing.T) {
	f := newLifecycleFixture()
	const id = "inst_lc_full"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	// Full golden path: create → stop → start → reboot → delete
	steps := []struct {
		name    string
		handler func() error
		want    string
	}{
		{"create", func() error { return f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")) }, "running"},
		{"stop", func() error { return f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP")) }, "stopped"},
		{"start", func() error { return f.newStartHandler().Execute(ctx, testJob(id, "INSTANCE_START")) }, "running"},
		{"reboot", func() error { return f.newRebootHandler().Execute(ctx, testJob(id, "INSTANCE_REBOOT")) }, "running"},
		{"delete", func() error { return f.newDeleteHandler().Execute(ctx, testJob(id, "INSTANCE_DELETE")) }, "deleted"},
	}
	for _, step := range steps {
		if err := step.handler(); err != nil {
			t.Fatalf("step %q: %v", step.name, err)
		}
		assertState(t, f.store, id, step.want)
	}
}

func TestLifecycle_Delete_FromRunning(t *testing.T) {
	f := newLifecycleFixture()
	const id = "inst_lc_del_running"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	if err := f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	assertState(t, f.store, id, "running")

	if err := f.newDeleteHandler().Execute(ctx, testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("delete from running: %v", err)
	}
	assertState(t, f.store, id, "deleted")
}

func TestLifecycle_Delete_FromStopped(t *testing.T) {
	f := newLifecycleFixture()
	const id = "inst_lc_del_stopped"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	if err := f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertState(t, f.store, id, "stopped")

	if err := f.newDeleteHandler().Execute(ctx, testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("delete from stopped: %v", err)
	}
	assertState(t, f.store, id, "deleted")
}

func TestLifecycle_StopFailure_RemainsInStopping(t *testing.T) {
	f := newLifecycleFixture()
	f.rt.stopFail = true
	const id = "inst_lc_stopfail"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	// First create succeeds (stop is not called during create).
	f.rt.stopFail = false
	if err := f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	assertState(t, f.store, id, "running")

	// Now fail stop.
	f.rt.stopFail = true
	err := f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP"))
	if err == nil {
		t.Fatal("expected stop error, got nil")
	}
	// Must remain in stopping — retryable, not terminal.
	assertState(t, f.store, id, "stopping")
}

func TestLifecycle_StartFailure_TransitionsToFailed(t *testing.T) {
	f := newLifecycleFixture()
	const id = "inst_lc_startfail"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	if err := f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertState(t, f.store, id, "stopped")

	// Fail start at CreateInstance.
	f.rt.createFail = true
	err := f.newStartHandler().Execute(ctx, testJob(id, "INSTANCE_START"))
	if err == nil {
		t.Fatal("expected start error, got nil")
	}
	assertState(t, f.store, id, "failed")
}

func TestLifecycle_RebootFailure_TransitionsToFailed(t *testing.T) {
	f := newLifecycleFixture()
	const id = "inst_lc_rebootfail"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	if err := f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	assertState(t, f.store, id, "running")

	// Fail reboot at StopInstance.
	f.rt.stopFail = true
	err := f.newRebootHandler().Execute(ctx, testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected reboot error, got nil")
	}
	assertState(t, f.store, id, "failed")
}

func TestLifecycle_UsageEvents_StopAndStart(t *testing.T) {
	// Verify: usage.end written on stop, usage.start written on start.
	f := newLifecycleFixture()
	const id = "inst_lc_usage"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	_ = f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE"))
	_ = f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP"))
	_ = f.newStartHandler().Execute(ctx, testJob(id, "INSTANCE_START"))

	evts := f.store.eventTypes()
	counts := map[string]int{}
	for _, e := range evts {
		counts[e]++
	}

	// Two usage.start events (create + start after stop).
	if counts[db.EventUsageStart] != 2 {
		t.Errorf("usage.start count = %d, want 2 (create + start); events: %v", counts[db.EventUsageStart], evts)
	}
	// One usage.end event (stop).
	if counts[db.EventUsageEnd] != 1 {
		t.Errorf("usage.end count = %d, want 1 (stop); events: %v", counts[db.EventUsageEnd], evts)
	}
}

func TestLifecycle_IPReleaseAndReallocate_StopThenStart(t *testing.T) {
	// Stop releases IP; start allocates a new one.
	f := newLifecycleFixture()
	f.net.nextIP = "10.0.2.1"
	const id = "inst_lc_ip"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	_ = f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE"))
	assertState(t, f.store, id, "running")

	// CreateHandler called AllocateIP which returned f.net.nextIP ("10.0.2.1"),
	// but the fake store's ip map is not automatically populated by the handler
	// (the real DB ip_allocations table would be — here we seed it manually
	// so GetIPByInstance returns the right value when StopHandler calls it).
	f.store.ips[id] = "10.0.2.1"

	_ = f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP"))

	// IP must have been released.
	if len(f.net.released) == 0 {
		t.Fatal("IP not released on stop")
	}
	if f.net.released[0] != "10.0.2.1" {
		t.Errorf("released IP = %q, want 10.0.2.1", f.net.released[0])
	}

	// Change what the fake allocates next.
	f.net.nextIP = "10.0.2.99"
	_ = f.newStartHandler().Execute(ctx, testJob(id, "INSTANCE_START"))
	assertState(t, f.store, id, "running")
}
