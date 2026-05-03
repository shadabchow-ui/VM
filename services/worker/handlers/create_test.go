package handlers

// create_test.go — Unit tests for CreateHandler and DeleteHandler.
//
// Uses in-memory fakes that directly implement InstanceStore and NetworkController.
// No real DB, no real Host Agent, no real network controller.
// readinessFn is overridden to return immediately (no TCP dial).
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.
//
// M10 Slice 3: fakeStore extended with root disk methods to satisfy the updated
// InstanceStore interface. Four new tests added for root disk lifecycle wiring.

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
	ips       map[string]string          // instanceID → ip
	rootDisks map[string]*db.RootDiskRow // diskID → disk (M10 Slice 3)
	sshKeys   map[string]*db.SSHKeyRow   // keyed by "principalID:name"
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		instances: make(map[string]*db.InstanceRow),
		ips:       make(map[string]string),
		rootDisks: make(map[string]*db.RootDiskRow),
		sshKeys:   make(map[string]*db.SSHKeyRow),
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

// ── Root disk methods (M10 Slice 3) ──────────────────────────────────────────
// These satisfy the InstanceStore interface extension added in Slice 3.
// Source: 06-01-root-disk-model-and-persistence-semantics.md.

func (s *fakeStore) CreateRootDisk(_ context.Context, row *db.RootDiskRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	s.rootDisks[row.DiskID] = row
	return nil
}

func (s *fakeStore) GetRootDiskByInstanceID(_ context.Context, instanceID string) (*db.RootDiskRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, disk := range s.rootDisks {
		if disk.InstanceID != nil && *disk.InstanceID == instanceID {
			cp := *disk
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *fakeStore) UpdateRootDiskStatus(_ context.Context, diskID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	disk, ok := s.rootDisks[diskID]
	if !ok {
		return fmt.Errorf("disk %s not found", diskID)
	}
	disk.Status = status
	return nil
}

func (s *fakeStore) DeleteRootDisk(_ context.Context, diskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rootDisks, diskID)
	return nil
}

func (s *fakeStore) DetachRootDisk(_ context.Context, diskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	disk, ok := s.rootDisks[diskID]
	if !ok {
		return fmt.Errorf("disk %s not found", diskID)
	}
	disk.InstanceID = nil
	disk.Status = db.RootDiskStatusDetached
	return nil
}

// ── NIC lifecycle stubs (VM-P2A-S2) ──────────────────────────────────────────
// These three methods satisfy the InstanceStore interface extension.
// fakeStore does not track NIC state — it relies on fakeNICStore (nic_lifecycle_test.go)
// for NIC-specific assertions. Here the stubs return nil, nil / nil so that
// Phase 1 classic-instance tests (which have no NIC rows seeded) treat all
// NIC calls as safe no-ops, matching the "nil → no-op" contract in the handlers.

func (s *fakeStore) GetPrimaryNetworkInterfaceByInstance(_ context.Context, _ string) (*db.NetworkInterfaceRow, error) {
	return nil, nil // no NIC seeded in plain fakeStore → Phase 1 classic path
}

func (s *fakeStore) UpdateNetworkInterfaceStatus(_ context.Context, _, _ string) error {
	return nil // no-op: NIC state tracking is in fakeNICStore
}

func (s *fakeStore) SoftDeleteNetworkInterface(_ context.Context, _ string) error {
	return nil // no-op: NIC state tracking is in fakeNICStore
}

// ── Volume attachment stubs (VM Job 8) ───────────────────────────────────────

func (s *fakeStore) ListActiveAttachmentsByInstance(_ context.Context, _ string) ([]*db.VolumeAttachmentRow, error) {
	return nil, nil // no active attachments in plain fakeStore
}

func (s *fakeStore) GetVolumeByID(_ context.Context, _ string) (*db.VolumeRow, error) {
	return nil, nil // no volumes seeded in plain fakeStore
}

// ── SSH key retrieval (VM-RUNTIME-BOOT-LANE-PHASE-A-B-C) ────────────────────

func (s *fakeStore) GetSSHKeyByPrincipalName(_ context.Context, principalID, name string) (*db.SSHKeyRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s:%s", principalID, name)
	if row, ok := s.sshKeys[key]; ok {
		cp := *row
		return &cp, nil
	}
	return nil, nil
}

// ── SG rule retrieval (VM Job 4) ──────────────────────────────────────────────

func (s *fakeStore) GetEffectiveSGRulesForInstance(_ context.Context, _ string) ([]db.EffectiveSGRuleRow, error) {
	return nil, nil // no SG rules seeded in plain fakeStore
}

// ── helpers ───────────────────────────────────────────────────────────────────

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

// getDisk returns the root disk for an instance ID (for test assertions).
func (s *fakeStore) getDiskByInstance(instanceID string) *db.RootDiskRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, disk := range s.rootDisks {
		if disk.InstanceID != nil && *disk.InstanceID == instanceID {
			cp := *disk
			return &cp
		}
	}
	return nil
}

// getDiskByID returns a disk by disk ID (for test assertions).
func (s *fakeStore) getDiskByID(diskID string) *db.RootDiskRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	disk, ok := s.rootDisks[diskID]
	if !ok {
		return nil
	}
	cp := *disk
	return &cp
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
	mu            sync.Mutex
	createFail    bool
	deletedInsts  []string
	lastCreateReq *runtimeclient.CreateInstanceRequest // track for SSH key assertions
	lastDeleteReq *runtimeclient.DeleteInstanceRequest // M10 Slice 3: capture delete flags
}

func (r *fakeRuntime) CreateInstance(_ context.Context, req *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastCreateReq = req
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
	reqCopy := *req
	r.lastDeleteReq = &reqCopy
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
		VMState:          "requested", InstanceTypeID: "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		Version:          0, CreatedAt: now, UpdatedAt: now,
	}
}

// instantReadiness always returns nil — no TCP dial.
func instantReadiness(_ context.Context, _ string, _ time.Duration) error { return nil }

func newTestCreateHandler(store *fakeStore, net *fakeNetwork, rt *fakeRuntime) *CreateHandler {
	deps := &Deps{
		Store:        store,
		Network:      net,
		DefaultVPCID: phase1VPCID,
		Runtime:      func(_, _ string) RuntimeClient { return nil }, // not called; overridden below
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
		Runtime:      func(_, _ string) RuntimeClient { return nil },
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

// ── M10 Slice 3: Root disk lifecycle wiring tests ─────────────────────────────
//
// Source: 06-01-root-disk-model-and-persistence-semantics.md §Delete Semantics,
//         P2_VOLUME_MODEL.md §1.

// TestCreateHandler_RootDiskCreatedAndAttached verifies that a successful
// INSTANCE_CREATE creates a root_disks record in CREATING status and advances
// it to ATTACHED once the VM is running.
func TestCreateHandler_RootDiskCreatedAndAttached(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.20"}
	rt := &fakeRuntime{}

	const id = "inst_disk_create"
	store.instances[id] = newRequestedInstance(id)

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Instance must be running.
	if store.instances[id].VMState != "running" {
		t.Errorf("vm_state = %q, want running", store.instances[id].VMState)
	}

	// A root disk record must exist with status ATTACHED.
	disk := store.getDiskByInstance(id)
	if disk == nil {
		t.Fatal("no root disk record created for instance")
	}
	if disk.Status != db.RootDiskStatusAttached {
		t.Errorf("disk status = %q, want ATTACHED", disk.Status)
	}
	if !disk.DeleteOnTermination {
		t.Error("delete_on_termination should be true (Phase 1 default)")
	}
	if disk.SourceImageID != "00000000-0000-0000-0000-000000000010" {
		t.Errorf("source_image_id = %q, want instance image ID", disk.SourceImageID)
	}
	if disk.StoragePoolID != phase1StoragePoolID {
		t.Errorf("storage_pool_id = %q, want phase1StoragePoolID", disk.StoragePoolID)
	}
}

// TestCreateHandler_RootDisk_Idempotent verifies that re-running INSTANCE_CREATE
// when a root disk already exists (job retry) does not create a duplicate.
func TestCreateHandler_RootDisk_Idempotent(t *testing.T) {
	store := newFakeStore()
	store.hosts = []*db.HostRecord{newReadyHost()}
	net := &fakeNetwork{nextIP: "10.0.0.21"}
	rt := &fakeRuntime{}

	const id = "inst_disk_retry"
	inst := newRequestedInstance(id)
	inst.VMState = "provisioning"
	inst.Version = 1
	store.instances[id] = inst

	// Pre-seed a CREATING disk as if step 3b ran but the job died before step 5.
	diskID := rootDiskIDFromInstance(id)
	instIDCopy := id
	store.rootDisks[diskID] = &db.RootDiskRow{
		DiskID:              diskID,
		InstanceID:          &instIDCopy,
		SourceImageID:       inst.ImageID,
		StoragePoolID:       phase1StoragePoolID,
		StoragePath:         "nfs://filer/vol/" + diskID + ".qcow2",
		SizeGB:              50,
		DeleteOnTermination: true,
		Status:              db.RootDiskStatusCreating,
	}

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute on retry: %v", err)
	}

	// Exactly one disk record should exist.
	count := 0
	for _, disk := range store.rootDisks {
		if disk.InstanceID != nil && *disk.InstanceID == id {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want 1 root disk record, got %d", count)
	}

	// Disk must be ATTACHED after successful run.
	disk := store.getDiskByInstance(id)
	if disk == nil {
		t.Fatal("root disk record not found after retry")
	}
	if disk.Status != db.RootDiskStatusAttached {
		t.Errorf("disk status = %q after retry, want ATTACHED", disk.Status)
	}
}

// TestDeleteHandler_DeleteOnTerminationTrue_DiskDeleted verifies that when
// delete_on_termination=true, the root disk DB record is removed after instance
// deletion and DeleteRootDisk=true is passed to the host agent.
func TestDeleteHandler_DeleteOnTerminationTrue_DiskDeleted(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_dot_true"
	hostID := "host-001"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = &hostID
	inst.Version = 2
	store.instances[id] = inst

	// Seed a root disk with delete_on_termination=true.
	diskID := rootDiskIDFromInstance(id)
	instIDCopy := id
	store.rootDisks[diskID] = &db.RootDiskRow{
		DiskID:              diskID,
		InstanceID:          &instIDCopy,
		SourceImageID:       inst.ImageID,
		StoragePoolID:       phase1StoragePoolID,
		StoragePath:         "nfs://filer/vol/" + diskID + ".qcow2",
		SizeGB:              50,
		DeleteOnTermination: true,
		Status:              db.RootDiskStatusAttached,
	}

	h := newTestDeleteHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Instance must be deleted.
	if store.instances[id].VMState != "deleted" {
		t.Errorf("vm_state = %q, want deleted", store.instances[id].VMState)
	}

	// Root disk DB record must be gone.
	if d := store.getDiskByID(diskID); d != nil {
		t.Errorf("root disk record should be deleted when delete_on_termination=true, got status=%q", d.Status)
	}

	// Host agent must have been told to delete the root disk file.
	if rt.lastDeleteReq == nil {
		t.Fatal("no DeleteInstance call recorded")
	}
	if !rt.lastDeleteReq.DeleteRootDisk {
		t.Error("DeleteRootDisk=false sent to host agent, want true for delete_on_termination=true")
	}
}

// TestDeleteHandler_DeleteOnTerminationFalse_DiskDetached verifies that when
// delete_on_termination=false, the root disk DB record is detached (not deleted)
// and DeleteRootDisk=false is passed to the host agent, preserving the qcow2 file.
// This is the Phase 2 persistent volume entry point.
// Source: P2_VOLUME_MODEL.md §1, 06-01-root-disk-model-and-persistence-semantics.md.
func TestDeleteHandler_DeleteOnTerminationFalse_DiskDetached(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_dot_false"
	hostID := "host-001"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = &hostID
	inst.Version = 2
	store.instances[id] = inst

	// Seed a root disk with delete_on_termination=false.
	diskID := rootDiskIDFromInstance(id)
	instIDCopy := id
	store.rootDisks[diskID] = &db.RootDiskRow{
		DiskID:              diskID,
		InstanceID:          &instIDCopy,
		SourceImageID:       inst.ImageID,
		StoragePoolID:       phase1StoragePoolID,
		StoragePath:         "nfs://filer/vol/" + diskID + ".qcow2",
		SizeGB:              100,
		DeleteOnTermination: false, // retain as detached volume
		Status:              db.RootDiskStatusAttached,
	}

	h := newTestDeleteHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Instance must be deleted.
	if store.instances[id].VMState != "deleted" {
		t.Errorf("vm_state = %q, want deleted", store.instances[id].VMState)
	}

	// Root disk DB record must still exist with DETACHED status and no instance_id.
	d := store.getDiskByID(diskID)
	if d == nil {
		t.Fatal("root disk record should be retained when delete_on_termination=false")
	}
	if d.Status != db.RootDiskStatusDetached {
		t.Errorf("disk status = %q, want DETACHED", d.Status)
	}
	if d.InstanceID != nil {
		t.Errorf("disk instance_id = %q, want nil after detach", *d.InstanceID)
	}

	// Host agent must have been told NOT to delete the root disk file.
	if rt.lastDeleteReq == nil {
		t.Fatal("no DeleteInstance call recorded")
	}
	if rt.lastDeleteReq.DeleteRootDisk {
		t.Error("DeleteRootDisk=true sent to host agent, want false for delete_on_termination=false")
	}
}

// TestDeleteHandler_NoDiskRecord_StillCompletes verifies backward compatibility:
// instances created before Slice 3 have no root_disks row. The delete handler
// must complete without error, defaulting to DeleteRootDisk=true for the host agent.
func TestDeleteHandler_NoDiskRecord_StillCompletes(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_no_disk"
	hostID := "host-001"
	inst := newRequestedInstance(id)
	inst.VMState = "running"
	inst.HostID = &hostID
	inst.Version = 2
	store.instances[id] = inst
	// No root disk seeded — simulates pre-Slice-3 instance.

	h := newTestDeleteHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_DELETE")); err != nil {
		t.Fatalf("Execute with no disk record: %v", err)
	}

	// Instance must be deleted.
	if store.instances[id].VMState != "deleted" {
		t.Errorf("vm_state = %q, want deleted", store.instances[id].VMState)
	}

	// Host agent must have been called with DeleteRootDisk=true (safe default).
	if rt.lastDeleteReq == nil {
		t.Fatal("no DeleteInstance call recorded")
	}
	if !rt.lastDeleteReq.DeleteRootDisk {
		t.Error("DeleteRootDisk=false, want true as default for instances with no disk record")
	}
}

// M3 stub tests removed — stop/start/reboot are fully implemented.
// Full test coverage is in stop_test.go, start_test.go, reboot_test.go.

// ── SSH key flow tests (VM-RUNTIME-BOOT-LANE-PHASE-A-B-C) ──────────────────

// TestCreateHandler_SSHKeyResolvedAndPassed verifies the SSH key is resolved
// and passed to the runtime when the instance has a matching ssh_key_name.
func TestCreateHandler_SSHKeyResolvedAndPassed(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_ssh_key"
	const principal = "princ_alice"
	const keyName = "my-key"
	const publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI..."

	// Seed SSH key.
	store.sshKeys[principal+":"+keyName] = &db.SSHKeyRow{
		ID:          "key_001",
		PrincipalID: principal,
		Name:        keyName,
		PublicKey:   publicKey,
	}

	inst := newRequestedInstance(id)
	inst.SSHKeyName = keyName
	inst.OwnerPrincipalID = principal
	store.instances[id] = inst
	store.hosts = []*db.HostRecord{{ID: "host-ssh"}}

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if rt.lastCreateReq == nil {
		t.Fatal("CreateInstance was not called")
	}
	if rt.lastCreateReq.SSHPublicKey != publicKey {
		t.Errorf("SSHPublicKey = %q, want %q", rt.lastCreateReq.SSHPublicKey, publicKey)
	}
	if rt.lastCreateReq.Hostname != id {
		t.Errorf("Hostname = %q, want %q", rt.lastCreateReq.Hostname, id)
	}
}

// TestCreateHandler_SSHKeyNotFound_NonFatal verifies that a missing SSH key
// does not block provisioning. The instance boots without SSH key injection.
func TestCreateHandler_SSHKeyNotFound_NonFatal(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_no_key"
	const principal = "princ_alice"

	inst := newRequestedInstance(id)
	inst.SSHKeyName = "nonexistent-key"
	inst.OwnerPrincipalID = principal
	store.instances[id] = inst
	store.hosts = []*db.HostRecord{{ID: "host-nokey"}}

	// SSH key not seeded — lookup returns nil.

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute with missing SSH key should succeed: %v", err)
	}

	if rt.lastCreateReq == nil {
		t.Fatal("CreateInstance was not called")
	}
	if rt.lastCreateReq.SSHPublicKey != "" {
		t.Errorf("SSHPublicKey = %q, want empty (key not found)", rt.lastCreateReq.SSHPublicKey)
	}
}

// TestCreateHandler_NoSSHKeyName_EmptyKey verifies that when ssh_key_name is
// empty, no SSH key resolution is attempted and an empty key is passed.
func TestCreateHandler_NoSSHKeyName_EmptyKey(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_no_name"

	inst := newRequestedInstance(id)
	inst.SSHKeyName = "" // no key specified
	store.instances[id] = inst
	store.hosts = []*db.HostRecord{{ID: "host-empty"}}

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if rt.lastCreateReq == nil {
		t.Fatal("CreateInstance was not called")
	}
	if rt.lastCreateReq.SSHPublicKey != "" {
		t.Errorf("SSHPublicKey = %q, want empty", rt.lastCreateReq.SSHPublicKey)
	}
}

// TestCreateHandler_RootDiskIdempotent verifies root disk creation is idempotent
// on job retry — the second create call reuses the existing disk record.
func TestCreateHandler_RootDiskIdempotent(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_rt_idem"

	inst := newRequestedInstance(id)
	store.instances[id] = inst
	store.hosts = []*db.HostRecord{{ID: "host-rt"}}

	h := newTestCreateHandler(store, net, rt)

	// First call creates the disk.
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("first Execute: %v", err)
	}

	// Verify disk was created.
	disk := store.getDiskByInstance(id)
	if disk == nil {
		t.Fatal("root disk was not created")
	}
	wantPath := "nfs://filer/vol/disk_" + id + ".qcow2"
	if disk.StoragePath != wantPath {
		t.Errorf("StoragePath = %q, want %q", disk.StoragePath, wantPath)
	}

	// Reset instance state and re-run (simulates job retry before instance reaches running).
	store.instances[id].VMState = "requested"
	store.instances[id].HostID = nil
	store.instances[id].Version = 0

	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err != nil {
		t.Fatalf("retry Execute: %v", err)
	}

	// Root disk should still be the same (idempotent — no duplicate).
	disk2 := store.getDiskByInstance(id)
	if disk2 == nil {
		t.Fatal("root disk was lost on retry")
	}
	// DiskID is deterministic: "disk_" + instance_id
	if disk2.DiskID != disk.DiskID {
		t.Errorf("DiskID changed: %q → %q", disk.DiskID, disk2.DiskID)
	}
}

// TestCreateHandler_RollbackCleansUp verifies that when CreateInstance fails
// on the runtime, the handler rolls back by releasing the IP and marking the
// instance as failed.
func TestCreateHandler_RollbackCleansUp(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}
	rt.createFail = true

	const id = "inst_rollback"

	inst := newRequestedInstance(id)
	store.instances[id] = inst
	store.hosts = []*db.HostRecord{{ID: "host-rb"}}

	h := newTestCreateHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_CREATE")); err == nil {
		t.Fatal("expected error from failed CreateInstance, got nil")
	}

	// Instance must be in failed state.
	if store.instances[id].VMState != "failed" {
		t.Errorf("vm_state = %q, want failed", store.instances[id].VMState)
	}

	// IP must be released.
	if len(net.released) == 0 {
		t.Error("IP was not released during rollback")
	}
}

// TestSSHKeyFlow_StartHandler verifies the start handler also resolves SSH keys.
func TestSSHKeyFlow_StartHandler(t *testing.T) {
	store := newFakeStore()
	net := &fakeNetwork{}
	rt := &fakeRuntime{}

	const id = "inst_start_ssh"
	const principal = "princ_alice"
	const keyName = "start-key"
	const publicKey = "ssh-rsa AAAAB3NzaC1yc2E..."

	store.sshKeys[principal+":"+keyName] = &db.SSHKeyRow{
		ID:          "key_start",
		PrincipalID: principal,
		Name:        keyName,
		PublicKey:   publicKey,
	}

	inst := newRequestedInstance(id)
	inst.VMState = "stopped"
	inst.SSHKeyName = keyName
	inst.OwnerPrincipalID = principal
	store.instances[id] = inst
	store.ips[id] = "10.0.5.1"
	store.hosts = []*db.HostRecord{{ID: "host-start"}}

	h := newTestStartHandler(store, net, rt)
	if err := h.Execute(context.Background(), testJob(id, "INSTANCE_START")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if rt.lastCreateReq == nil {
		t.Fatal("CreateInstance was not called")
	}
	if rt.lastCreateReq.SSHPublicKey != publicKey {
		t.Errorf("SSHPublicKey = %q, want %q", rt.lastCreateReq.SSHPublicKey, publicKey)
	}
}
