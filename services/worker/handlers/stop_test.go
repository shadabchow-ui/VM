package handlers

// stop_test.go — Unit tests for StopHandler.
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

	"github.com/compute-platform/compute-platform/internal/db"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// ── fakeStopRuntime — extends fakeRuntime with stop-specific failure controls ──

type fakeStopRuntime struct {
	stopFail   bool
	deleteFail bool
	stopCalled []string
}

func (r *fakeStopRuntime) CreateInstance(_ context.Context, req *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error) {
	return &runtimeclient.CreateInstanceResponse{InstanceID: req.InstanceID, State: "RUNNING"}, nil
}

func (r *fakeStopRuntime) StopInstance(_ context.Context, req *runtimeclient.StopInstanceRequest) (*runtimeclient.StopInstanceResponse, error) {
	r.stopCalled = append(r.stopCalled, req.InstanceID)
	if r.stopFail {
		return nil, errors.New("fakeStopRuntime: StopInstance failure")
	}
	return &runtimeclient.StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"}, nil
}

func (r *fakeStopRuntime) DeleteInstance(_ context.Context, req *runtimeclient.DeleteInstanceRequest) (*runtimeclient.DeleteInstanceResponse, error) {
	if r.deleteFail {
		return nil, errors.New("fakeStopRuntime: DeleteInstance failure")
	}
	return &runtimeclient.DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"}, nil
}


func newTestStopHandler(store *fakeStore, net *fakeNetwork, rt RuntimeClient) *StopHandler {
	deps := &Deps{
		Store:        store,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) *runtimeclient.Client { return nil },
	}
	h := NewStopHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return rt }
	return h
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestStopHandler_HappyPath_TransitionsToStopped(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{}

	const id = "inst_stop001"
	inst := newRunningInstance(id)
	store.instances[id] = inst
	store.ips[id] = "10.0.0.5"

	h := newTestStopHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if store.instances[id].VMState != "stopped" {
		t.Errorf("vm_state = %q, want stopped", store.instances[id].VMState)
	}
}

func TestStopHandler_IPRetained(t *testing.T) {
	// IP_ALLOCATION_CONTRACT_V1 §5: private IP is stable across stop/start.
	// Stop does NOT release the IP; the ip_allocations row is kept so the
	// same IP can be reused when the instance is started again.
	// Only the delete handler releases the IP.
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{}

	const id = "inst_stop_ip"
	store.instances[id] = newRunningInstance(id)
	store.ips[id] = "10.0.0.20"

	h := newTestStopHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_STOP"))

	// IP must NOT be released on stop — retained per IP_ALLOCATION_CONTRACT_V1 §5.
	if len(net.released) != 0 {
		t.Errorf("IP released on stop (got %v) — violates IP_ALLOCATION_CONTRACT_V1 §5 (private IP retained across stop/start)", net.released)
	}
	// IP record must still be present in the store.
	if store.ips[id] == "" {
		t.Error("IP cleared from store on stop — must be retained for start reuse")
	}
}

func TestStopHandler_UsageEndEventWritten(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{}

	const id = "inst_stop_evt"
	store.instances[id] = newRunningInstance(id)

	h := newTestStopHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_STOP"))

	evts := store.eventTypes()
	found := false
	for _, e := range evts {
		if e == db.EventUsageEnd {
			found = true
		}
	}
	if !found {
		t.Errorf("usage.end event not written; got %v", evts)
	}
}

func TestStopHandler_StopInitiateEventWritten(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{}

	const id = "inst_stop_initevt"
	store.instances[id] = newRunningInstance(id)

	h := newTestStopHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_STOP"))

	evts := store.eventTypes()
	found := false
	for _, e := range evts {
		if e == db.EventInstanceStopInitiate {
			found = true
		}
	}
	if !found {
		t.Errorf("stop.initiate event not written; got %v", evts)
	}
}

func TestStopHandler_IllegalState_Provisioning_ReturnsError(t *testing.T) {
	store := newFakeStore()
	const id = "inst_stop_illegal"
	inst := newRequestedInstance(id)
	inst.VMState = "provisioning"
	store.instances[id] = inst

	h := newTestStopHandler(store, &fakeNetwork{}, &fakeStopRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP"))
	if err == nil {
		t.Fatal("expected error for illegal state provisioning, got nil")
	}
	// Must not have mutated state.
	if store.instances[id].VMState != "provisioning" {
		t.Errorf("state mutated to %q; illegal transition must not write state", store.instances[id].VMState)
	}
}

func TestStopHandler_IllegalState_Deleted_ReturnsError(t *testing.T) {
	// deleted is handled as no-op (not an error), per idempotency contract.
	store := newFakeStore()
	const id = "inst_stop_deleted"
	inst := newRequestedInstance(id)
	inst.VMState = "deleted"
	store.instances[id] = inst

	h := newTestStopHandler(store, &fakeNetwork{}, &fakeStopRuntime{})
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP")); err != nil {
		t.Errorf("Execute on deleted = %v, want nil (idempotent no-op)", err)
	}
}

func TestStopHandler_IllegalState_Failed_ReturnsError(t *testing.T) {
	store := newFakeStore()
	const id = "inst_stop_failed"
	inst := newRequestedInstance(id)
	inst.VMState = "failed"
	store.instances[id] = inst

	h := newTestStopHandler(store, &fakeNetwork{}, &fakeStopRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP"))
	if err == nil {
		t.Fatal("expected error for illegal state failed, got nil")
	}
}

func TestStopHandler_AlreadyStopped_IsNoOp(t *testing.T) {
	store := newFakeStore()
	const id = "inst_stop_noop"
	inst := newRunningInstance(id)
	inst.VMState = "stopped"
	store.instances[id] = inst

	h := newTestStopHandler(store, &fakeNetwork{}, &fakeStopRuntime{})
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP")); err != nil {
		t.Errorf("Execute on stopped = %v, want nil (idempotent no-op)", err)
	}
	// No state mutation.
	if store.instances[id].VMState != "stopped" {
		t.Errorf("state changed = %q, want stopped", store.instances[id].VMState)
	}
	// No events written.
	if len(store.events) != 0 {
		t.Errorf("events written on no-op: %v", store.eventTypes())
	}
}

func TestStopHandler_DuplicateDelivery_ReentrantInStopping(t *testing.T) {
	// Re-entrant: job delivered again while instance is already in stopping.
	// Handler must resume from runtime ops and complete to stopped.
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{}

	const id = "inst_stop_reentrant"
	inst := newRunningInstance(id)
	inst.VMState = "stopping"
	inst.Version = 3
	store.instances[id] = inst
	store.ips[id] = "10.0.0.33"

	h := newTestStopHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("Execute re-entrant stop: %v", err)
	}
	if store.instances[id].VMState != "stopped" {
		t.Errorf("vm_state = %q, want stopped", store.instances[id].VMState)
	}
}

func TestStopHandler_StopInstanceFailure_ReturnsError(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{stopFail: true}

	const id = "inst_stop_rtfail"
	store.instances[id] = newRunningInstance(id)

	h := newTestStopHandler(store, net, rt)
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP"))
	if err == nil {
		t.Fatal("expected error when StopInstance fails, got nil")
	}
	// State must remain stopping (not failed) — retryable error.
	if store.instances[id].VMState != "stopping" {
		t.Errorf("vm_state = %q after StopInstance failure, want stopping (retryable)", store.instances[id].VMState)
	}
}

func TestStopHandler_DeleteInstanceFailure_ReturnsError(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{deleteFail: true}

	const id = "inst_stop_delfail"
	store.instances[id] = newRunningInstance(id)

	h := newTestStopHandler(store, net, rt)
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP"))
	if err == nil {
		t.Fatal("expected error when DeleteInstance fails, got nil")
	}
	// State must remain stopping — retryable.
	if store.instances[id].VMState != "stopping" {
		t.Errorf("vm_state = %q after DeleteInstance failure, want stopping", store.instances[id].VMState)
	}
}

func TestStopHandler_NoHost_SkipsRuntimeOps(t *testing.T) {
	// Instance with no host assigned (e.g. failed during provisioning before host
	// was set). Stop should release IP and transition to stopped without calling
	// any host-agent ops.
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeStopRuntime{}

	const id = "inst_stop_nohost"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = nil
	inst.Version = 1
	store.instances[id] = inst

	h := newTestStopHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("Execute with no host: %v", err)
	}
	if store.instances[id].VMState != "stopped" {
		t.Errorf("vm_state = %q, want stopped", store.instances[id].VMState)
	}
	// Runtime was never called.
	if len(rt.stopCalled) != 0 {
		t.Errorf("StopInstance called despite no host: %v", rt.stopCalled)
	}
}
