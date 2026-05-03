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
		Runtime: func(_, _ string) RuntimeClient { return nil }}
	h := NewCreateHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	h.readinessFn = instantReadiness
	return h
}

func (f *lifecycleFixture) newStopHandler() *StopHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) RuntimeClient { return nil }}
	h := NewStopHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	return h
}

func (f *lifecycleFixture) newStartHandler() *StartHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) RuntimeClient { return nil }}
	h := NewStartHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	h.readinessFn = instantReadiness
	return h
}

func (f *lifecycleFixture) newRebootHandler() *RebootHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) RuntimeClient { return nil }}
	h := NewRebootHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return f.rt }
	h.readinessFn = instantReadiness
	return h
}

func (f *lifecycleFixture) newDeleteHandler() *DeleteHandler {
	deps := &Deps{Store: f.store, Network: f.net, DefaultVPCID: phase1VPCID,
		Runtime: func(_, _ string) RuntimeClient { return nil }}
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

// newRunningInstance constructs an *db.InstanceRow already in "running" state
// with a host assigned. Used by reboot tests and any test that needs to start
// from a live instance without running the full create provisioning sequence.
func newRunningInstance(id string) *db.InstanceRow {
	inst := newRequestedInstance(id)
	hostID := "host-001"
	inst.VMState = "running"
	inst.HostID = &hostID
	inst.Version = 1
	return inst
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
	// Seed retained IP: stop no longer releases it; start reads it via GetIPByInstance.
	f.store.ips[id] = f.net.nextIP

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
	// Seed retained IP after create so stop and start both find it via GetIPByInstance.
	steps := []struct {
		name  string
		setup func() // optional pre-step hook
		exec  func() error
		want  string
	}{
		{
			name:  "create",
			setup: func() {},
			exec:  func() error { return f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE")) },
			want:  "running",
		},
		{
			// Seed retained IP before stop so GetIPByInstance (stop step 6 NIC path)
			// and start step 5 (GetIPByInstance for retained IP) both work.
			name:  "stop",
			setup: func() { f.store.ips[id] = f.net.nextIP },
			exec:  func() error { return f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP")) },
			want:  "stopped",
		},
		{
			name:  "start",
			setup: func() {},
			exec:  func() error { return f.newStartHandler().Execute(ctx, testJob(id, "INSTANCE_START")) },
			want:  "running",
		},
		{
			name:  "reboot",
			setup: func() {},
			exec:  func() error { return f.newRebootHandler().Execute(ctx, testJob(id, "INSTANCE_REBOOT")) },
			want:  "running",
		},
		{
			name:  "delete",
			setup: func() {},
			exec:  func() error { return f.newDeleteHandler().Execute(ctx, testJob(id, "INSTANCE_DELETE")) },
			want:  "deleted",
		},
	}
	for _, step := range steps {
		step.setup()
		if err := step.exec(); err != nil {
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
	// Seed retained IP: stop no longer releases it; delete needs it for ReleaseIP.
	f.store.ips[id] = f.net.nextIP
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
	// Seed retained IP before stop so start can find it via GetIPByInstance.
	f.store.ips[id] = f.net.nextIP
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
	// Retained IP must still be present: start failure does not release it
	// (the IP stays reserved for the next start attempt per IP_ALLOCATION_CONTRACT_V1 §5).
	if f.store.ips[id] == "" {
		t.Error("retained IP cleared on start failure — should remain for retry")
	}
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
	f.store.ips[id] = f.net.nextIP // seed retained IP so stop and start both work
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

func TestLifecycle_IP_RetainedThroughStopStart(t *testing.T) {
	// IP_ALLOCATION_CONTRACT_V1 §5: private IP is stable across stop/start.
	// Stop does NOT release the IP; start reuses the retained allocation.
	f := newLifecycleFixture()
	f.net.nextIP = "10.0.2.1"
	const id = "inst_lc_ip"
	f.store.instances[id] = newRequestedInstance(id)
	ctx := context.Background()

	_ = f.newCreateHandler().Execute(ctx, testJob(id, "INSTANCE_CREATE"))
	assertState(t, f.store, id, "running")

	// Seed the retained IP so stop's GetPrimaryNetworkInterfaceByInstance
	// and start's GetIPByInstance can find it (mirrors the real DB state
	// where ip_allocations.owner_instance_id remains set after stop).
	f.store.ips[id] = "10.0.2.1"

	_ = f.newStopHandler().Execute(ctx, testJob(id, "INSTANCE_STOP"))
	assertState(t, f.store, id, "stopped")

	// IP must NOT have been released on stop.
	if len(f.net.released) != 0 {
		t.Errorf("IP released on stop (got %v) — violates IP_ALLOCATION_CONTRACT_V1 §5 (private IP retained)", f.net.released)
	}
	if f.store.ips[id] == "" {
		t.Error("IP cleared from store on stop — should be retained")
	}

	// Start reuses the retained IP — no new AllocateIP call on the main path.
	// We verify this by confirming net.released does not grow during start
	// (retained IP is reused, not released and re-allocated).
	releasedBeforeStart := len(f.net.released)
	_ = f.newStartHandler().Execute(ctx, testJob(id, "INSTANCE_START"))
	assertState(t, f.store, id, "running")

	// No IP released during start.
	if len(f.net.released) != releasedBeforeStart {
		t.Errorf("start released IP(s) %v — retained IP should be reused with no release",
			f.net.released[releasedBeforeStart:])
	}
	// The retained IP value is unchanged.
	if f.store.ips[id] != "10.0.2.1" {
		t.Errorf("IP after start = %q, want 10.0.2.1 (retained)", f.store.ips[id])
	}
}
