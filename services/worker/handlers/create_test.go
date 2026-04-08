package handlers

// create_test.go — Unit tests for CreateHandler and DeleteHandler.
//
// Uses in-memory fakes that directly implement InstanceStore and NetworkController.
// No real DB, no real Host Agent, no real network controller.
// readinessFn is overridden to return immediately (no TCP dial).
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// ── fakeStore — implements InstanceStore ──────────────────────────────────────

type fakeStore struct {
	mu        sync.Mutex
	instances map[string]*db.InstanceRow
	events    []*db.EventRow
	hosts     []*db.HostRecord
	ips       map[string]string // instanceID → ip
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		instances: make(map[string]*db.InstanceRow),
		ips:       make(map[string]string),
	}
}

func (s *fakeStore) GetInstanceByID(_ context.Context, id string) (*db.InstanceRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	if !ok {
		return nil, fmt.Errorf("instance %s not found", id)
	}
	cp := *inst
	return &cp, nil
}

func (s *fakeStore) UpdateInstanceState(_ context.Context, id, expectedState, newState string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	if inst.VMState != expectedState {
		return fmt.Errorf("state mismatch: have %q, expected %q", inst.VMState, expectedState)
	}
	if inst.Version != version {
		return fmt.Errorf("version mismatch: have %d, expected %d", inst.Version, version)
	}
	inst.VMState = newState
	inst.Version++
	return nil
}

func (s *fakeStore) AssignHost(_ context.Context, instanceID, hostID string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[instanceID]
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}
	if inst.Version != version {
		return fmt.Errorf("version mismatch")
	}
	inst.HostID = &hostID
	inst.Version++
	return nil
}

func (s *fakeStore) SoftDeleteInstance(_ context.Context, id string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	if inst.Version != version {
		return fmt.Errorf("SoftDelete version mismatch: have %d, expected %d", inst.Version, version)
	}
	inst.VMState = "deleted"
	now := time.Now()
	inst.DeletedAt = &now
	inst.Version++
	return nil
}

func (s *fakeStore) GetAvailableHosts(_ context.Context) ([]*db.HostRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hosts, nil
}

func (s *fakeStore) InsertEvent(_ context.Context, row *db.EventRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, row)
	return nil
}

func (s *fakeStore) GetIPByInstance(_ context.Context, instanceID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ips[instanceID], nil
}

// eventTypes returns the list of event types written so far (for assertions).
func (s *fakeStore) eventTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.EventType
	}
	return out
}

// ── fakeNetwork — implements NetworkController ────────────────────────────────

type fakeNetwork struct {
	mu       sync.Mutex
	nextIP   string
	failNext bool
	released []string
}

func (n *fakeNetwork) AllocateIP(_ context.Context, _, _ string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failNext {
		n.failNext = false
		return "", errors.New("fakeNetwork: allocation failure")
	}
	if n.nextIP == "" {
		n.nextIP = "10.0.0.1"
	}
	return n.nextIP, nil
}

func (n *fakeNetwork) ReleaseIP(_ context.Context, ip, _, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.released = append(n.released, ip)
	return nil
}

// ── fakeRuntimeClient — implements RuntimeClient ──────────────────────────────

type fakeRuntime struct {
	mu           sync.Mutex
	createFail   bool
	deletedInsts []string
}

func (r *fakeRuntime) CreateInstance(_ context.Context, req *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createFail {
		return nil, errors.New("fakeRuntime: CreateInstance failure")
	}
	return &runtimeclient.CreateInstanceResponse{InstanceID: req.InstanceID, State: "RUNNING"}, nil
}

func (r *fakeRuntime) StopInstance(_ context.Context, req *runtimeclient.StopInstanceRequest) (*runtimeclient.StopInstanceResponse, error) {
	return &runtimeclient.StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"}, nil
}

func (r *fakeRuntime) DeleteInstance(_ context.Context, req *runtimeclient.DeleteInstanceRequest) (*runtimeclient.DeleteInstanceResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletedInsts = append(r.deletedInsts, req.InstanceID)
	return &runtimeclient.DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"}, nil
}

// ── Test helpers ──────────────────────────────────────────────────────────────

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testJob(instanceID, jobType string) *db.JobRow {
	return &db.JobRow{ID: "job-001", InstanceID: instanceID, JobType: jobType, AttemptCount: 1, MaxAttempts: 3}
}

func newReadyHost() *db.HostRecord {
	now := time.Now()
	return &db.HostRecord{
		ID: "host-001", AvailabilityZone: "us-east-1a", Status: "ready",
		TotalCPU: 8, TotalMemoryMB: 16384, TotalDiskGB: 200,
		AgentVersion: "v0.1.0-m2", LastHeartbeatAt: &now,
		RegisteredAt: now, UpdatedAt: now,
	}
}

func newRequestedInstance(id string) *db.InstanceRow {
	now := time.Now()
	return &db.InstanceRow{
		ID: id, Name: "test-vm",
		OwnerPrincipalID: "00000000-0000-0000-0000-000000000001",
		VMState: "requested", InstanceTypeID: "c1.small",
		ImageID: "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		Version: 0, CreatedAt: now, UpdatedAt: now,
	}
}

// instantReadiness always returns nil — no TCP dial.
func instantReadiness(_ context.Context, _ string, _ time.Duration) error { return nil }

func newTestCreateHandler(store *fakeStore, net *fakeNetwork, rt *fakeRuntime) *CreateHandler {
	deps := &Deps{
		Store:        store,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) *runtimeclient.Client { return nil }, // not called; overridden below
	}
	h := NewCreateHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return rt }
	h.readinessFn = instantReadiness
	return h
}

func newTestDeleteHandler(store *fakeStore, net *fakeNetwork, rt *fakeRuntime) *DeleteHandler {
	deps := &Deps{
		Store:        store,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) *runtimeclient.Client { return nil },
	}
	h := NewDeleteHandler(deps, testLog())
	h.runtimeFactory = func(_, _ string) RuntimeClient { return rt }
	return h
}

// ── CreateHandler tests ───────────────────────────────────────────────────────

func TestCreateHandler_HappyPath_TransitionsToRunning(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.5"}
	rt := &fakeRuntime{}

	const id = "inst_create001"
	store.instances[id] = newRequestedInstance(id)

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	inst := store.instances[id]
	if inst.VMState != "running" {
		t.Errorf("vm_state = %q, want running", inst.VMState)
	}
	if inst.HostID == nil || *inst.HostID != "host-001" {
		t.Errorf("host_id = %v, want host-001", inst.HostID)
	}
	if len(store.events) == 0 {
		t.Error("no events written")
	}
}

func TestCreateHandler_NoHosts_TransitionsToFailed(t *testing.T) {
	store := newFakeStore()
	store.hosts = nil
	const id = "inst_nohost"
	store.instances[id] = newRequestedInstance(id)

	h := newTestCreateHandler(store, &fakeNetwork{}, &fakeRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE"))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after no-host failure, want failed", store.instances[id].VMState)
	}
}

func TestCreateHandler_IPAllocFailure_TransitionsToFailed(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{failNext: true}
	const id = "inst_noip"
	store.instances[id] = newRequestedInstance(id)

	h := newTestCreateHandler(store, net, &fakeRuntime{})
	err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE"))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q after IP failure, want failed", store.instances[id].VMState)
	}
}

func TestCreateHandler_CreateInstanceFailure_ReleasesIPAndFails(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.7"}
	rt := &fakeRuntime{createFail: true}
	const id = "inst_createfail"
	store.instances[id] = newRequestedInstance(id)

	h := newTestCreateHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE"))

	if len(net.released) == 0 {
		t.Error("IP not released during rollback after CreateInstance failure")
	}
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q, want failed", store.instances[id].VMState)
	}
}

func TestCreateHandler_AlreadyProvisioning_Idempotent(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.9"}
	rt := &fakeRuntime{}
	const id = "inst_retry"
	inst := newRequestedInstance(id)
	inst.VMState = "provisioning"
	inst.Version = 1
	store.instances[id] = inst

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute on provisioning instance: %v", err)
	}
	if store.instances[id].VMState != "running" {
		t.Errorf("vm_state = %q, want running", store.instances[id].VMState)
	}
}

func TestCreateHandler_EventsWritten(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.11"}
	rt := &fakeRuntime{}
	const id = "inst_events"
	store.instances[id] = newRequestedInstance(id)

	h := newTestCreateHandler(store, net, rt)
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE"))

	evts := store.eventTypes()
	contains := func(s string) bool {
		for _, e := range evts {
			if e == s {
				return true
			}
		}
		return false
	}
	if !contains(db.EventInstanceProvisioningStart) {
		t.Errorf("missing event %q; got %v", db.EventInstanceProvisioningStart, evts)
	}
	if !contains(db.EventUsageStart) {
		t.Errorf("missing event %q; got %v", db.EventUsageStart, evts)
	}
	if !contains(db.EventIPAllocated) {
		t.Errorf("missing event %q; got %v", db.EventIPAllocated, evts)
	}
}

// ── DeleteHandler tests ───────────────────────────────────────────────────────

func TestDeleteHandler_HappyPath_TransitionsToDeleted(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	const id = "inst_del001"
	hostID := "host-001"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = &hostID
	inst.Version = 2
	store.instances[id] = inst
	store.ips[id] = "10.0.0.3"

	h := newTestDeleteHandler(store, net, &fakeRuntime{})
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if store.instances[id].VMState != "deleted" {
		t.Errorf("vm_state = %q, want deleted", store.instances[id].VMState)
	}
}

func TestDeleteHandler_AlreadyDeleted_IsNoOp(t *testing.T) {
	store := newFakeStore()
	const id = "inst_alreadydel"
	inst := newRequestedInstance(id)
	now := time.Now()
	inst.VMState = "deleted"
	inst.DeletedAt = &now
	store.instances[id] = inst

	h := newTestDeleteHandler(store, &fakeNetwork{}, &fakeRuntime{})
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Errorf("Execute on already-deleted = %v, want nil (idempotent)", err)
	}
}

func TestDeleteHandler_IPReleased(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	const id = "inst_iprel"
	hostID := "host-001"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = &hostID
	inst.Version = 1
	store.instances[id] = inst
	store.ips[id] = "10.0.0.42"

	h := newTestDeleteHandler(store, net, &fakeRuntime{})
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_DELETE"))

	if len(net.released) == 0 {
		t.Error("IP not released during delete")
		return
	}
	if net.released[0] != "10.0.0.42" {
		t.Errorf("released IP = %q, want 10.0.0.42", net.released[0])
	}
}

func TestDeleteHandler_UsageEndEventWritten(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	const id = "inst_usageend"
	hostID := "host-001"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = &hostID
	inst.Version = 1
	store.instances[id] = inst

	h := newTestDeleteHandler(store, net, &fakeRuntime{})
	_ = h.Execute(context.Background(), testJob(id, "INSTANCE_DELETE"))

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

// M3 stub tests removed — stop/start/reboot are fully implemented.
// Full test coverage is in stop_test.go, start_test.go, reboot_test.go.
