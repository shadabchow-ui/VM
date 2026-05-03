package handlers

// nic_lifecycle_test.go — VM-P2A-S2 NIC lifecycle unit tests.
//
// Tests NIC status transitions across the full instance lifecycle for VPC
// instances and verifies Phase 1 classic instances are unaffected.
//
// Requires fakeNICStore (defined below) which embeds *fakeStore and overrides
// the three NIC methods to provide real state tracking in memory.
//
// These tests cover:
//   - Create: NIC starts "pending" in handler; worker advances to "attached"
//   - Stop: NIC advanced to "detached"; private IP retained in ip_allocations
//   - Start: NIC advanced back to "attached"; retained IP reused (no new alloc)
//   - Delete: NIC soft-deleted after IP release
//   - Rollback: NIC stays "pending" if CreateInstance fails
//   - Phase 1 classic: all NIC paths are no-ops
//
// Source: VM-P2A-S2 audit findings R1, R2, R3, R6, R7.

import (
	"context"
	"errors"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// ── fakeNICStore ──────────────────────────────────────────────────────────────

// fakeNICStore wraps *fakeStore and adds in-memory NIC state tracking.
// It overrides the three NIC methods on InstanceStore so VPC lifecycle tests
// can assert NIC status transitions without a real database.
type fakeNICStore struct {
	*fakeStore
	// nics maps nicID → NIC row (mutable status field).
	nics map[string]*db.NetworkInterfaceRow
	// nicByInstance maps instanceID → primary nicID.
	nicByInstance map[string]string
}

func newFakeNICStore(base *fakeStore) *fakeNICStore {
	return &fakeNICStore{
		fakeStore:     base,
		nics:          make(map[string]*db.NetworkInterfaceRow),
		nicByInstance: make(map[string]string),
	}
}

// seedNIC seeds a NIC row as the handler would after S2: status="pending", PrivateIP="".
func (s *fakeNICStore) seedNIC(instanceID, nicID, subnetID, vpcID string) {
	nic := &db.NetworkInterfaceRow{
		ID:         nicID,
		InstanceID: instanceID,
		SubnetID:   subnetID,
		VPCID:      vpcID,
		PrivateIP:  "",
		MACAddress: "02:aa:bb:cc:dd:ee",
		IsPrimary:  true,
		Status:     "pending",
	}
	s.nics[nicID] = nic
	s.nicByInstance[instanceID] = nicID
}

func (s *fakeNICStore) GetPrimaryNetworkInterfaceByInstance(_ context.Context, instanceID string) (*db.NetworkInterfaceRow, error) {
	nicID, ok := s.nicByInstance[instanceID]
	if !ok {
		return nil, nil
	}
	nic, ok := s.nics[nicID]
	if !ok {
		return nil, nil
	}
	copy := *nic // return a copy so callers cannot mutate state directly
	return &copy, nil
}

func (s *fakeNICStore) UpdateNetworkInterfaceStatus(_ context.Context, nicID, status string) error {
	nic, ok := s.nics[nicID]
	if !ok {
		return nil // idempotent no-op
	}
	nic.Status = status
	return nil
}

func (s *fakeNICStore) SoftDeleteNetworkInterface(_ context.Context, nicID string) error {
	nic, ok := s.nics[nicID]
	if !ok {
		return nil // idempotent no-op
	}
	nic.Status = "deleted"
	return nil
}

// nicStatus is a test helper that reads NIC status from the store.
func (s *fakeNICStore) nicStatus(instanceID string) string {
	nicID, ok := s.nicByInstance[instanceID]
	if !ok {
		return ""
	}
	nic, ok := s.nics[nicID]
	if !ok {
		return ""
	}
	return nic.Status
}

// ── fakeFailRuntime ───────────────────────────────────────────────────────────

// fakeFailRuntime returns an error on CreateInstance; other ops succeed.
// Used to test rollback paths.
type fakeFailRuntime struct{}

func (r *fakeFailRuntime) CreateInstance(_ context.Context, _ *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error) {
	return nil, errors.New("fakeFailRuntime: CreateInstance injected failure")
}
func (r *fakeFailRuntime) StopInstance(_ context.Context, req *runtimeclient.StopInstanceRequest) (*runtimeclient.StopInstanceResponse, error) {
	return &runtimeclient.StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"}, nil
}
func (r *fakeFailRuntime) DeleteInstance(_ context.Context, req *runtimeclient.DeleteInstanceRequest) (*runtimeclient.DeleteInstanceResponse, error) {
	return &runtimeclient.DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

const (
	testNICID    = "nic_test_001"
	testSubnetID = "subnet_test"
	testVPCID    = "vpc_test"
)

// newVPCFixture builds a lifecycle fixture with a fakeNICStore pre-seeded
// with a NIC reservation for the given instance ID.
func newVPCFixture(instanceID string) (*fakeNICStore, *fakeNetwork) {
	base := newFakeStore()
	base.hosts = []*db.HostRecord{newReadyHost()}
	base.instances[instanceID] = newRequestedInstance(instanceID)

	store := newFakeNICStore(base)
	store.seedNIC(instanceID, testNICID, testSubnetID, testVPCID)

	net := &fakeNetwork{nextIP: "10.0.1.50"}
	return store, net
}

func makeDepsNIC(store InstanceStore, net *fakeNetwork) *Deps {
	return &Deps{
		Store:        store,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) RuntimeClient { return nil },
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestNIC_Create_AdvancesToAttached verifies that after INSTANCE_CREATE
// completes, the primary NIC status is "attached".
// Guards audit finding R3 (NIC was permanently stuck in "attaching").
func TestNIC_Create_AdvancesToAttached(t *testing.T) {
	const id = "inst_nic_create"
	store, net := newVPCFixture(id)
	rt := &fakeFullRuntime{}

	deps := makeDepsNIC(store, net)
	h := NewCreateHandler(deps, testLog())
	h.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	h.readinessFn = instantReadiness

	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := store.nicStatus(id); got != "attached" {
		t.Errorf("NIC status after create = %q, want attached", got)
	}
	if store.instances[id].VMState != "running" {
		t.Errorf("vm_state = %q, want running", store.instances[id].VMState)
	}
}

// TestNIC_Stop_AdvancesToDetached verifies that after INSTANCE_STOP the NIC
// status is "detached" and the Phase 1 flat-pool IP is NOT released.
// Guards audit finding R1 (stop was releasing IP) and R3 (NIC not updated).
func TestNIC_Stop_AdvancesToDetached(t *testing.T) {
	const id = "inst_nic_stop"
	store, net := newVPCFixture(id)
	rt := &fakeFullRuntime{}
	deps := makeDepsNIC(store, net)

	// Create first to get the instance running.
	ch := NewCreateHandler(deps, testLog())
	ch.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	ch.readinessFn = instantReadiness
	if err := ch.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Seed retained IP in store (simulates allocation from create step).
	store.ips[id] = net.nextIP

	sh := NewStopHandler(deps, testLog())
	sh.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	if err := sh.Execute(context.Background(), testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// NIC must be detached.
	if got := store.nicStatus(id); got != "detached" {
		t.Errorf("NIC status after stop = %q, want detached", got)
	}

	// IP must still be retained in store (R1: stop does NOT release IP).
	if store.ips[id] == "" {
		t.Error("IP was released on stop — violates IP_ALLOCATION_CONTRACT_V1 §5 (private IP retained on stop)")
	}
}

// TestNIC_Start_AfterStop_ReattachesNIC verifies that after INSTANCE_START
// following a stop, the NIC returns to "attached" and the retained IP is reused.
// Guards audit finding R1 (start was re-allocating) and R3 (NIC not updated).
func TestNIC_Start_AfterStop_ReattachesNIC(t *testing.T) {
	const id = "inst_nic_start"
	store, net := newVPCFixture(id)
	rt := &fakeFullRuntime{}
	deps := makeDepsNIC(store, net)

	// Create.
	ch := NewCreateHandler(deps, testLog())
	ch.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	ch.readinessFn = instantReadiness
	if err := ch.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	store.ips[id] = net.nextIP
	ipAfterCreate := net.nextIP

	// Stop.
	sh := NewStopHandler(deps, testLog())
	sh.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	if err := sh.Execute(context.Background(), testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	store.instances[id].VMState = "stopped"

	// Start.
	starth := NewStartHandler(deps, testLog())
	starth.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	starth.readinessFn = instantReadiness
	if err := starth.Execute(context.Background(), testJob(id, "INSTANCE_START")); err != nil {
		t.Fatalf("start: %v", err)
	}

	// NIC must be re-attached.
	if got := store.nicStatus(id); got != "attached" {
		t.Errorf("NIC status after start = %q, want attached", got)
	}

	// IP must be the same retained IP — no re-allocation (R1).
	if store.ips[id] != ipAfterCreate {
		t.Errorf("IP changed after start: was %q, now %q — IP should be retained across stop/start",
			ipAfterCreate, store.ips[id])
	}
}

// TestNIC_Delete_SoftDeletesNIC verifies that after INSTANCE_DELETE the primary
// NIC row is soft-deleted (status → "deleted"). This is the NIC-specific
// invariant; IP release on delete is covered by delete handler tests elsewhere
// (TestIdempotency_INSTANCE_DELETE_ResourcesFreedOnce, TestLifecycle_Delete_*).
// Guards audit finding R3.
func TestNIC_Delete_SoftDeletesNIC(t *testing.T) {
	const id = "inst_nic_delete"
	store, net := newVPCFixture(id)
	rt := &fakeFullRuntime{}
	deps := makeDepsNIC(store, net)

	// Create.
	ch := NewCreateHandler(deps, testLog())
	ch.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	ch.readinessFn = instantReadiness
	if err := ch.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed retained IP so delete can find it via GetIPByInstance.
	store.ips[id] = net.nextIP

	// Delete directly from running.
	dh := NewDeleteHandler(deps, testLog())
	dh.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	if err := dh.Execute(context.Background(), testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// NIC must be soft-deleted — this is the NIC lifecycle assertion.
	if got := store.nicStatus(id); got != "deleted" {
		t.Errorf("NIC status after delete = %q, want deleted", got)
	}

	// Instance must reach deleted state.
	if store.instances[id].VMState != "deleted" {
		t.Errorf("vm_state after delete = %q, want deleted", store.instances[id].VMState)
	}
	// Note: IP release on delete is verified by TestIdempotency_INSTANCE_DELETE_ResourcesFreedOnce
	// and TestLifecycle_Delete_* — not duplicated here.
}

// TestNIC_Create_Rollback_NICStaysPending verifies that if CreateInstance fails
// on the host agent, the NIC status remains "pending" (not "attached").
// This ensures the NIC reservation is left in a consistent retryable state.
func TestNIC_Create_Rollback_NICStaysPending(t *testing.T) {
	const id = "inst_nic_rollback"
	store, net := newVPCFixture(id)
	rt := &fakeFailRuntime{} // CreateInstance always fails

	deps := makeDepsNIC(store, net)
	h := NewCreateHandler(deps, testLog())
	h.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	h.readinessFn = instantReadiness

	err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE"))
	if err == nil {
		t.Fatal("expected error from CreateInstance failure, got nil")
	}

	// NIC must still be "pending" — not "attaching" or "attached".
	// The worker did not get past CreateInstance so the NIC was never updated.
	if got := store.nicStatus(id); got != "pending" {
		t.Errorf("NIC status after rollback = %q, want pending", got)
	}

	// Instance must be failed.
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state after rollback = %q, want failed", store.instances[id].VMState)
	}
}

// TestNIC_Phase1Classic_NoNICPaths verifies that Phase 1 classic instances
// (no NIC row seeded) run through the full create→stop→start→delete lifecycle
// without error. All NIC calls gate on nic != nil.
func TestNIC_Phase1Classic_NoNICPaths(t *testing.T) {
	const id = "inst_classic"

	// Use plain fakeStore — no NIC seeded.
	base := newFakeStore()
	base.hosts = []*db.HostRecord{newReadyHost()}
	base.instances[id] = newRequestedInstance(id)
	net := &fakeNetwork{nextIP: "10.0.2.1"}
	rt := &fakeFullRuntime{}

	deps := &Deps{
		Store:        base,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) RuntimeClient { return nil },
	}

	ctx := context.Background()

	// Create.
	ch := NewCreateHandler(deps, testLog())
	ch.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	ch.readinessFn = instantReadiness
	if err := ch.Execute(ctx, testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if base.instances[id].VMState != "running" {
		t.Errorf("vm_state after create = %q, want running", base.instances[id].VMState)
	}

	base.ips[id] = net.nextIP

	// Stop.
	sh := NewStopHandler(deps, testLog())
	sh.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	if err := sh.Execute(ctx, testJob(id, "INSTANCE_STOP")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	base.instances[id].VMState = "stopped"

	// Start.
	starth := NewStartHandler(deps, testLog())
	starth.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	starth.readinessFn = instantReadiness
	if err := starth.Execute(ctx, testJob(id, "INSTANCE_START")); err != nil {
		t.Fatalf("start: %v", err)
	}
	base.instances[id].VMState = "running"
	base.ips[id] = net.nextIP

	// Delete.
	dh := NewDeleteHandler(deps, testLog())
	dh.SetRuntimeFactory(func(_, _ string) RuntimeClient { return rt })
	if err := dh.Execute(ctx, testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if base.instances[id].VMState != "deleted" {
		t.Errorf("vm_state after delete = %q, want deleted", base.instances[id].VMState)
	}
}
