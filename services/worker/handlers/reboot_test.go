package handlers

// reboot_test.go — Unit tests for RebootHandler.
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

// ── fakeRebootRuntime — stop and create failure controls ─────────────────────

type fakeRebootRuntime struct {
	stopFail     bool
	createFail   bool
	stopCalled   []string
	createCalled []string
	deleteCalled []string
}

func (r *fakeRebootRuntime) StopInstance(_ context.Context, req *runtimeclient.StopInstanceRequest) (*runtimeclient.StopInstanceResponse, error) {
	r.stopCalled = append(r.stopCalled, req.InstanceID)
	if r.stopFail {
		return nil, errors.New("fakeRebootRuntime: StopInstance failure")
	}
	return &runtimeclient.StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"}, nil
}

func (r *fakeRebootRuntime) CreateInstance(_ context.Context, req *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error) {
	r.createCalled = append(r.createCalled, req.InstanceID)
	if r.createFail {
		return nil, errors.New("fakeRebootRuntime: CreateInstance failure")
	}
	return &runtimeclient.CreateInstanceResponse{InstanceID: req.InstanceID, State: "RUNNING"}, nil
}

func (r *fakeRebootRuntime) DeleteInstance(_ context.Context, req *runtimeclient.DeleteInstanceRequest) (*runtimeclient.DeleteInstanceResponse, error) {
	r.deleteCalled = append(r.deleteCalled, req.InstanceID)
	return &runtimeclient.DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestRebootHandler(store *fakeStore, net *fakeNetwork, rt RuntimeClient) *RebootHandler {
	deps := &Deps{
		Store:        store,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) RuntimeClient { return nil },
	}
	h := NewRebootHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return rt }
	h.readinessFn = instantReadiness
	return h
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestRebootHandler_HappyPath_TransitionsToRunning(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot001"
	inst := newRunningInstance(id)
	store.instances[id] = inst
	store.ips[id] = "10.0.0.70"

	h := newTestRebootHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if store.instances[id].VMState != "running" {
		t.Errorf("vm_state = %q, want running", store.instances[id].VMState)
	}
}

func TestRebootHandler_SameHost_RetainedAfterReboot(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_host"
	inst := newRunningInstance(id)
	// HostID is host-001 from newRunningInstance
	store.instances[id] = inst

	h := newTestRebootHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))

	if store.instances[id].HostID == nil || *store.instances[id].HostID != "host-001" {
		t.Errorf("host_id changed after reboot; got %v, want host-001", store.instances[id].HostID)
	}
}

func TestRebootHandler_IPNotReleased_DuringReboot(t *testing.T) {
	// Reboot retains the IP — ReleaseIP must never be called.
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_ip"
	inst := newRunningInstance(id)
	store.instances[id] = inst
	store.ips[id] = "10.0.0.71"

	h := newTestRebootHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))

	if len(net.released) != 0 {
		t.Errorf("IP released during reboot — must not release; got %v", net.released)
	}
}

func TestRebootHandler_DeleteInstance_NotCalled_DuringReboot(t *testing.T) {
	// Reboot does NOT call DeleteInstance — rootfs is preserved.
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_nodelete"
	store.instances[id] = newRunningInstance(id)

	h := newTestRebootHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))

	if len(rt.deleteCalled) != 0 {
		t.Errorf("DeleteInstance called during reboot — must not call; got %v", rt.deleteCalled)
	}
}

func TestRebootHandler_StopAndCreateBothCalled(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_rtcalls"
	store.instances[id] = newRunningInstance(id)

	h := newTestRebootHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))

	if len(rt.stopCalled) == 0 {
		t.Error("StopInstance not called during reboot")
	}
	if len(rt.createCalled) == 0 {
		t.Error("CreateInstance not called during reboot")
	}
}

func TestRebootHandler_RunningState_ExecutesFullReboot(t *testing.T) {
	// running is the valid fresh-entry state for reboot — the handler must
	// execute the full Stop + Create + readiness sequence, not no-op.
	// Contract: LIFECYCLE_STATE_MACHINE_V1 §2 RUNNING→REBOOTING→RUNNING.
	store := newFakeStore()
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_from_running"
	inst := newRunningInstance(id)
	store.instances[id] = inst

	h := newTestRebootHandler(store, &fakeNetwork{}, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT")); err != nil {
		t.Fatalf("Execute from running: %v", err)
	}
	// Both runtime ops must have been called.
	if len(rt.stopCalled) == 0 {
		t.Error("StopInstance not called during reboot from running")
	}
	if len(rt.createCalled) == 0 {
		t.Error("CreateInstance not called during reboot from running")
	}
	// Final state must be running.
	if store.instances[id].VMState != "running" {
		t.Errorf("vm_state = %q after reboot from running, want running", store.instances[id].VMState)
	}
}

func TestRebootHandler_IllegalState_Stopped_ReturnsError(t *testing.T) {
	store := newFakeStore()
	const id = "inst_reboot_stopped"
	inst := newRunningInstance(id)
	inst.VMState = "stopped"
	store.instances[id] = inst

	h := newTestRebootHandler(store, &fakeNetwork{}, &fakeRebootRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected error for illegal state stopped, got nil")
	}
	if store.instances[id].VMState != "stopped" {
		t.Errorf("state mutated to %q; illegal transition must not write state", store.instances[id].VMState)
	}
}

func TestRebootHandler_IllegalState_Provisioning_ReturnsError(t *testing.T) {
	store := newFakeStore()
	const id = "inst_reboot_prov"
	inst := newRequestedInstance(id)
	inst.VMState = "provisioning"
	store.instances[id] = inst

	h := newTestRebootHandler(store, &fakeNetwork{}, &fakeRebootRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected error for illegal state provisioning, got nil")
	}
}

func TestRebootHandler_IllegalState_Deleting_ReturnsError(t *testing.T) {
	store := newFakeStore()
	const id = "inst_reboot_deleting"
	inst := newRequestedInstance(id)
	inst.VMState = "deleting"
	store.instances[id] = inst

	h := newTestRebootHandler(store, &fakeNetwork{}, &fakeRebootRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected error for illegal state deleting, got nil")
	}
}

func TestRebootHandler_StopInstanceFailure_TransitionsToFailed(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{stopFail: true}

	const id = "inst_reboot_stopfail"
	store.instances[id] = newRunningInstance(id)

	h := newTestRebootHandler(store, net, rt)
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected error when StopInstance fails, got nil")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after StopInstance failure, want failed", store.instances[id].VMState)
	}
}

func TestRebootHandler_CreateInstanceFailure_TransitionsToFailed(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{createFail: true}

	const id = "inst_reboot_createfail"
	store.instances[id] = newRunningInstance(id)

	h := newTestRebootHandler(store, net, rt)
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected error when CreateInstance fails, got nil")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after CreateInstance failure, want failed", store.instances[id].VMState)
	}
}

func TestRebootHandler_ReadinessTimeout_TransitionsToFailed(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_timeout"
	store.instances[id] = newRunningInstance(id)

	h := newTestRebootHandler(store, net, rt)
	h.readinessFn = func(_ context.Context, _ string, _ time.Duration) error {
		return errors.New("readiness timeout")
	}
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected error on readiness timeout, got nil")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after readiness timeout, want failed", store.instances[id].VMState)
	}
}

func TestRebootHandler_DuplicateDelivery_ReentrantInRebooting(t *testing.T) {
	// Re-entrant: job delivered again while instance is already in rebooting.
	// Handler must resume from runtime ops and complete to running.
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_reentrant"
	inst := newRunningInstance(id)
	inst.VMState = "rebooting"
	inst.Version = 3
	store.instances[id] = inst
	store.ips[id] = "10.0.0.75"

	h := newTestRebootHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT")); err != nil {
		t.Fatalf("Execute re-entrant reboot: %v", err)
	}
	if store.instances[id].VMState != "running" {
		t.Errorf("vm_state = %q, want running", store.instances[id].VMState)
	}
}

func TestRebootHandler_RebootCompleteEventWritten(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_evt"
	store.instances[id] = newRunningInstance(id)

	h := newTestRebootHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))

	evts := store.eventTypes()
	found := false
	for _, e := range evts {
		if e == db.EventInstanceReboot {
			found = true
		}
	}
	if !found {
		t.Errorf("reboot event not written; got %v", evts)
	}
}

func TestRebootHandler_RebootInitiateEventWritten(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRebootRuntime{}

	const id = "inst_reboot_initevt"
	store.instances[id] = newRunningInstance(id)

	h := newTestRebootHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))

	evts := store.eventTypes()
	found := false
	for _, e := range evts {
		if e == db.EventInstanceRebootInitiate {
			found = true
		}
	}
	if !found {
		t.Errorf("reboot.initiate event not written; got %v", evts)
	}
}

func TestRebootHandler_NoHost_ReturnsError(t *testing.T) {
	// Reboot requires a host. No host = cannot reboot.
	store := newFakeStore()
	const id = "inst_reboot_nohost"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = nil
	inst.Version = 1
	store.instances[id] = inst

	h := newTestRebootHandler(store, &fakeNetwork{}, &fakeRebootRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_REBOOT"))
	if err == nil {
		t.Fatal("expected error when instance has no host, got nil")
	}
}
