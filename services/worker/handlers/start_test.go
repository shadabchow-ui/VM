package handlers

// start_test.go — Unit tests for StartHandler.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit,
//         LIFECYCLE_STATE_MACHINE_V1 §9 (required test matrix).
//
// All tests use in-memory fakes. No real DB, no real Host Agent.
// Tests are valid on macOS dev box — no Linux/KVM required.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// ── fakeStartRuntime — controls CreateInstance failure ───────────────────────

type fakeStartRuntime struct {
	createFail   bool
	createCalled []string
	deletedInsts []string
}

func (r *fakeStartRuntime) CreateInstance(_ context.Context, req *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error) {
	r.createCalled = append(r.createCalled, req.InstanceID)
	if r.createFail {
		return nil, errors.New("fakeStartRuntime: CreateInstance failure")
	}
	return &runtimeclient.CreateInstanceResponse{InstanceID: req.InstanceID, State: "RUNNING"}, nil
}

func (r *fakeStartRuntime) StopInstance(_ context.Context, req *runtimeclient.StopInstanceRequest) (*runtimeclient.StopInstanceResponse, error) {
	return &runtimeclient.StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"}, nil
}

func (r *fakeStartRuntime) DeleteInstance(_ context.Context, req *runtimeclient.DeleteInstanceRequest) (*runtimeclient.DeleteInstanceResponse, error) {
	r.deletedInsts = append(r.deletedInsts, req.InstanceID)
	return &runtimeclient.DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newStoppedInstance(id string) *db.InstanceRow {
	inst := newRequestedInstance(id)
	hostID := "host-001"
	inst.VMState = "stopped"
	inst.HostID = &hostID
	inst.Version = 4
	return inst
}

func newTestStartHandler(store *fakeStore, net *fakeNetwork, rt RuntimeClient) *StartHandler {
	deps := &Deps{
		Store:        store,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) RuntimeClient { return nil },
	}
	h := NewStartHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return rt }
	h.readinessFn = instantReadiness
	return h
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestStartHandler_HappyPath_TransitionsToRunning(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.50"}
	rt := &fakeStartRuntime{}

	const id = "inst_start001"
	store.instances[id] = newStoppedInstance(id)

	h := newTestStartHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_START")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	inst := store.instances[id]
	if inst.VMState != "running" {
		t.Errorf("vm_state = %q, want running", inst.VMState)
	}
	if inst.HostID == nil || *inst.HostID != "host-001" {
		t.Errorf("host_id = %v, want host-001", inst.HostID)
	}
}

func TestStartHandler_AlreadyRunning_IsNoOp(t *testing.T) {
	store := newFakeStore()
	const id = "inst_start_noop"
	inst := newStoppedInstance(id)
	inst.VMState = "running"
	store.instances[id] = inst

	h := newTestStartHandler(store, &fakeNetwork{}, &fakeStartRuntime{})
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_START")); err != nil {
		t.Errorf("Execute on running = %v, want nil (idempotent no-op)", err)
	}
	if store.instances[id].VMState != "running" {
		t.Errorf("state changed = %q", store.instances[id].VMState)
	}
	if len(store.events) != 0 {
		t.Errorf("events written on no-op: %v", store.eventTypes())
	}
}

func TestStartHandler_IllegalState_Running_ReturnsError(t *testing.T) {
	// "running" is handled as no-op, not an error. Test a truly illegal state.
	store := newFakeStore()
	const id = "inst_start_illegal"
	inst := newRequestedInstance(id)
	inst.VMState = "deleting"
	store.instances[id] = inst

	h := newTestStartHandler(store, &fakeNetwork{}, &fakeStartRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_START"))
	if err == nil {
		t.Fatal("expected error for illegal state deleting, got nil")
	}
	if store.instances[id].VMState != "deleting" {
		t.Errorf("state mutated to %q; illegal transition must not write state", store.instances[id].VMState)
	}
}

func TestStartHandler_IllegalState_Rebooting_ReturnsError(t *testing.T) {
	store := newFakeStore()
	const id = "inst_start_rebooting"
	inst := newRequestedInstance(id)
	inst.VMState = "rebooting"
	store.instances[id] = inst

	h := newTestStartHandler(store, &fakeNetwork{}, &fakeStartRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_START"))
	if err == nil {
		t.Fatal("expected error for illegal state rebooting, got nil")
	}
}

func TestStartHandler_NoHosts_TransitionsToFailed(t *testing.T) {
	store := newFakeStore()
	store.hosts = nil
	const id = "inst_start_nohost"
	store.instances[id] = newStoppedInstance(id)

	h := newTestStartHandler(store, &fakeNetwork{}, &fakeStartRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_START"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after no-host failure, want failed", store.instances[id].VMState)
	}
}

func TestStartHandler_IPAllocFailure_TransitionsToFailed(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{failNext: true}
	const id = "inst_start_noip"
	store.instances[id] = newStoppedInstance(id)

	h := newTestStartHandler(store, net, &fakeStartRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_START"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after IP failure, want failed", store.instances[id].VMState)
	}
}

func TestStartHandler_CreateInstanceFailure_ReleasesIPAndFails(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.51"}
	rt := &fakeStartRuntime{createFail: true}
	const id = "inst_start_createfail"
	store.instances[id] = newStoppedInstance(id)

	h := newTestStartHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_START"))

	if len(net.released) == 0 {
		t.Error("IP not released during rollback after CreateInstance failure")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q, want failed", store.instances[id].VMState)
	}
}

func TestStartHandler_ReadinessTimeout_RollsBackAndFails(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.52"}
	rt := &fakeStartRuntime{}
	const id = "inst_start_readiness"
	store.instances[id] = newStoppedInstance(id)

	h := newTestStartHandler(store, net, rt)
	h.readinessFn = func(_ context.Context, _ string, _ time.Duration) error {
		return errors.New("readiness timeout")
	}
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_START"))

	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after readiness timeout, want failed", store.instances[id].VMState)
	}
	// Rollback: IP must be released.
	if len(net.released) == 0 {
		t.Error("IP not released after readiness timeout rollback")
	}
	// Rollback: DeleteInstance must have been called.
	if len(rt.deletedInsts) == 0 {
		t.Error("DeleteInstance not called during readiness timeout rollback")
	}
}

func TestStartHandler_DuplicateDelivery_ReentrantInProvisioning(t *testing.T) {
	// Re-entrant: job delivered again while instance is already in provisioning.
	// Handler must resume and complete to running.
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.53"}
	rt := &fakeStartRuntime{}

	const id = "inst_start_reentrant"
	inst := newStoppedInstance(id)
	inst.VMState = "provisioning"
	inst.Version = 5
	store.instances[id] = inst

	h := newTestStartHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_START")); err != nil {
		t.Fatalf("Execute re-entrant start: %v", err)
	}
	if store.instances[id].VMState != "running" {
		t.Errorf("vm_state = %q, want running", store.instances[id].VMState)
	}
}

func TestStartHandler_UsageStartEventWritten(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.54"}
	rt := &fakeStartRuntime{}

	const id = "inst_start_evt"
	store.instances[id] = newStoppedInstance(id)

	h := newTestStartHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_START"))

	evts := store.eventTypes()
	found := false
	for _, e := range evts {
		if e == db.EventUsageStart {
			found = true
		}
	}
	if !found {
		t.Errorf("usage.start event not written; got %v", evts)
	}
}

func TestStartHandler_StartInitiateEventWritten(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.55"}
	rt := &fakeStartRuntime{}

	const id = "inst_start_initevt"
	store.instances[id] = newStoppedInstance(id)

	h := newTestStartHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_START"))

	evts := store.eventTypes()
	found := false
	for _, e := range evts {
		if e == db.EventInstanceStartInitiate {
			found = true
		}
	}
	if !found {
		t.Errorf("start.initiate event not written; got %v", evts)
	}
}

func TestStartHandler_HostReassigned_AfterStop(t *testing.T) {
	// After a stop, the prior host assignment is stale. Start must re-assign
	// host_id to the newly selected host.
	store := newFakeStore()
	newHost := newReadyHost()
	newHost.ID = "host-002" // different from the stopped instance's prior host-001
	store.hosts = []*db.HostRecord{newHost}
	net := &fakeNetwork{nextIP: "10.0.0.60"}
	rt := &fakeStartRuntime{}

	const id = "inst_start_rehost"
	inst := newStoppedInstance(id)
	// inst.HostID is "host-001" from newStoppedInstance
	store.instances[id] = inst

	h := newTestStartHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_START")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if store.instances[id].HostID == nil || *store.instances[id].HostID != "host-002" {
		t.Errorf("host_id = %v, want host-002 (re-assigned to new host)", store.instances[id].HostID)
	}
}
