package handlers

// volume_test.go — Unit tests for VolumeCreateHandler, VolumeAttachHandler,
// VolumeDetachHandler, VolumeDeleteHandler.
//
// Source: P2_VOLUME_MODEL.md §3 (state machine), §4 (attach/detach flows),
//         §5 (delete), §7 (invariants VOL-I-1, VOL-I-5),
//         vm-15-01__skill__independent-block-volume-architecture.md.
//         VM-P2B Slice 2.
//         VM-P2B-S3: added VOLUME_CREATE tests; extended fakeVolumeStore with
//           SetVolumeStoragePath and CountActiveSnapshotsByVolume.
//
// All tests use in-memory fakeVolumeStore. No real DB required.
//
// Test matrix:
//   VOLUME_CREATE
//     - success: creating → available, storage_path set
//     - idempotent: already available → no-op
//     - illegal state (available already): only if status != creating → error
//     - illegal state (deleting): error
//     - lock conflict: LockVolume fails → error, status remains creating
//     - SetVolumeStoragePath fails → unlocks to error
//     - re-entrant: locked_by == job.ID → skip LockVolume
//     - nil VolumeID → error
//   VOLUME_ATTACH
//     - success: available → attaching → in_use, attachment found
//     - idempotent: already in_use → no-op
//     - illegal state: deleting → error
//     - lock conflict: LockVolume fails → error, status remains available
//     - missing attachment: rolls back lock to available
//     - GetActiveAttachment fails: unlocks to error
//     - re-entrant: already attaching → skip lock, complete to in_use
//   VOLUME_DETACH
//     - success: in_use → detaching → available, attachment closed
//     - idempotent: already available → no-op
//     - illegal state: deleting → error
//     - lock conflict: LockVolume fails → error, status remains in_use
//     - CloseVolumeAttachment fails → unlocks to error
//     - re-entrant: already detaching → skip lock, complete to available
//     - re-entrant with closed attachment: no active row → still completes
//   VOLUME_DELETE
//     - success: available → deleting → deleted
//     - idempotent: already deleted → no-op
//     - idempotent: volume not found → no-op
//     - in_use rejected: error, status unchanged
//     - illegal state (creating): error
//     - lock conflict: error, status remains available
//     - SoftDeleteVolume fails → unlocks to error
//     - re-entrant: already deleting → skip lock, complete to deleted
//   Interface compliance: fakeVolumeStore satisfies VolumeStore (compile check)

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── fakeVolumeStore — implements VolumeStore ──────────────────────────────────

type fakeVolumeStore struct {
	mu          sync.Mutex
	volumes     map[string]*db.VolumeRow
	attachments map[string]*db.VolumeAttachmentRow // volumeID → active attachment (nil detached_at)

	// Fake snapshot counts for CountActiveSnapshotsByVolume.
	// volumeID → number of active snapshots.
	snapshotCounts map[string]int

	// Fault injection — set before Execute to simulate specific failures.
	lockFail           bool
	updateStatusFail   bool
	softDeleteFail     bool
	closeAttachFail    bool
	getAttachFail      bool
	setStoragePathFail bool
}

func newFakeVolumeStore() *fakeVolumeStore {
	return &fakeVolumeStore{
		volumes:        make(map[string]*db.VolumeRow),
		attachments:    make(map[string]*db.VolumeAttachmentRow),
		snapshotCounts: make(map[string]int),
	}
}

func (s *fakeVolumeStore) GetVolumeByID(_ context.Context, id string) (*db.VolumeRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[id]
	if !ok {
		return nil, nil // nil, nil matches real repo convention (no row = not found)
	}
	cp := *v
	return &cp, nil
}

func (s *fakeVolumeStore) LockVolume(_ context.Context, id, jobID, expectedStatus string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFail {
		return errors.New("fakeVolumeStore: LockVolume failure injected")
	}
	v, ok := s.volumes[id]
	if !ok {
		return fmt.Errorf("LockVolume: volume %s not found", id)
	}
	if v.Status != expectedStatus {
		return fmt.Errorf("LockVolume: status mismatch have %q expected %q", v.Status, expectedStatus)
	}
	if v.Version != version {
		return fmt.Errorf("LockVolume: version mismatch have %d expected %d", v.Version, version)
	}
	if v.LockedBy != nil {
		return fmt.Errorf("LockVolume: volume %s already locked by %s", id, *v.LockedBy)
	}
	v.LockedBy = &jobID
	v.Version++
	return nil
}

func (s *fakeVolumeStore) UnlockVolume(_ context.Context, id, newStatus string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[id]
	if !ok {
		return fmt.Errorf("UnlockVolume: volume %s not found", id)
	}
	v.LockedBy = nil
	v.Status = newStatus
	v.Version++
	return nil
}

func (s *fakeVolumeStore) UpdateVolumeStatus(_ context.Context, id, expectedStatus, newStatus string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateStatusFail {
		return errors.New("fakeVolumeStore: UpdateVolumeStatus failure injected")
	}
	v, ok := s.volumes[id]
	if !ok {
		return fmt.Errorf("UpdateVolumeStatus: volume %s not found", id)
	}
	if v.Status != expectedStatus {
		return fmt.Errorf("UpdateVolumeStatus: status mismatch have %q expected %q", v.Status, expectedStatus)
	}
	if v.Version != version {
		return fmt.Errorf("UpdateVolumeStatus: version mismatch have %d expected %d", v.Version, version)
	}
	v.Status = newStatus
	v.Version++
	return nil
}

func (s *fakeVolumeStore) SoftDeleteVolume(_ context.Context, id string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.softDeleteFail {
		return errors.New("fakeVolumeStore: SoftDeleteVolume failure injected")
	}
	v, ok := s.volumes[id]
	if !ok {
		return fmt.Errorf("SoftDeleteVolume: volume %s not found", id)
	}
	if v.Version != version {
		return fmt.Errorf("SoftDeleteVolume: version mismatch have %d expected %d", v.Version, version)
	}
	now := time.Now()
	v.Status = db.VolumeStatusDeleted
	v.DeletedAt = &now
	v.LockedBy = nil
	v.Version++
	return nil
}

func (s *fakeVolumeStore) GetActiveAttachmentByVolume(_ context.Context, volumeID string) (*db.VolumeAttachmentRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getAttachFail {
		return nil, errors.New("fakeVolumeStore: GetActiveAttachmentByVolume failure injected")
	}
	att, ok := s.attachments[volumeID]
	if !ok {
		return nil, nil
	}
	cp := *att
	return &cp, nil
}

func (s *fakeVolumeStore) CloseVolumeAttachment(_ context.Context, attachmentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeAttachFail {
		return errors.New("fakeVolumeStore: CloseVolumeAttachment failure injected")
	}
	for volID, att := range s.attachments {
		if att.ID == attachmentID {
			now := time.Now()
			att.DetachedAt = &now
			delete(s.attachments, volID)
			return nil
		}
	}
	return fmt.Errorf("CloseVolumeAttachment: attachment %s not found", attachmentID)
}

// SetVolumeStoragePath records storage_path on the volume.
// VM-P2B-S3: required by VolumeStore interface.
func (s *fakeVolumeStore) SetVolumeStoragePath(_ context.Context, id, storagePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setStoragePathFail {
		return errors.New("fakeVolumeStore: SetVolumeStoragePath failure injected")
	}
	v, ok := s.volumes[id]
	if !ok {
		return fmt.Errorf("SetVolumeStoragePath: volume %s not found", id)
	}
	v.StoragePath = &storagePath
	return nil
}

// CountActiveSnapshotsByVolume returns the injected snapshot count for a volume.
// VM-P2B-S3: required by VolumeStore interface.
func (s *fakeVolumeStore) CountActiveSnapshotsByVolume(_ context.Context, volumeID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotCounts[volumeID], nil
}

// ── compile-time interface check ──────────────────────────────────────────────

var _ VolumeStore = (*fakeVolumeStore)(nil)

// ── test fixtures ─────────────────────────────────────────────────────────────

func newAvailableVolume(id string) *db.VolumeRow {
	return &db.VolumeRow{
		ID:               id,
		OwnerPrincipalID: "principal-001",
		DisplayName:      "test-volume",
		Region:           "us-east-1",
		AvailabilityZone: "us-east-1a",
		SizeGB:           50,
		Origin:           "blank",
		Status:           db.VolumeStatusAvailable,
		Version:          1,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
}

func newCreatingVolumeBlank(id string) *db.VolumeRow {
	v := newAvailableVolume(id)
	v.Status = db.VolumeStatusCreating
	v.Version = 0
	return v
}

func newInUseVolume(id string) *db.VolumeRow {
	v := newAvailableVolume(id)
	v.Status = db.VolumeStatusInUse
	return v
}

func newAttachmentRow(volumeID, instanceID string) *db.VolumeAttachmentRow {
	return &db.VolumeAttachmentRow{
		ID:                  "vatt-001",
		VolumeID:            volumeID,
		InstanceID:          instanceID,
		DevicePath:          "/dev/vdb",
		DeleteOnTermination: false,
		AttachedAt:          time.Now(),
	}
}

// volumeJob builds a minimal JobRow for a volume-scoped job.
func volumeJob(volumeID, jobType string) *db.JobRow {
	return &db.JobRow{
		ID:           "job-vol-001",
		VolumeID:     &volumeID,
		JobType:      jobType,
		AttemptCount: 1,
		MaxAttempts:  5,
	}
}

func newTestCreateVolumeHandler(store *fakeVolumeStore) *VolumeCreateHandler {
	return NewVolumeCreateHandler(&VolumeDeps{Store: store}, testLog())
}

func newTestAttachHandler(store *fakeVolumeStore) *VolumeAttachHandler {
	return NewVolumeAttachHandler(&VolumeDeps{Store: store}, testLog())
}

func newTestDetachHandler(store *fakeVolumeStore) *VolumeDetachHandler {
	return NewVolumeDetachHandler(&VolumeDeps{Store: store}, testLog())
}

func newTestVolumeDeleteHandler(store *fakeVolumeStore) *VolumeDeleteHandler {
	return NewVolumeDeleteHandler(&VolumeDeps{Store: store}, testLog())
}

// assertVolumeStatus is a test helper for clean status assertions.
func assertVolumeStatus(t *testing.T, store *fakeVolumeStore, id, want string) {
	t.Helper()
	v := store.volumes[id]
	if v == nil {
		t.Fatalf("assertVolumeStatus: volume %s not found in store", id)
	}
	if v.Status != want {
		t.Errorf("volume %s status = %q, want %q", id, v.Status, want)
	}
}

func assertVolumeLocked(t *testing.T, store *fakeVolumeStore, id string, wantLocked bool) {
	t.Helper()
	v := store.volumes[id]
	if v == nil {
		t.Fatalf("assertVolumeLocked: volume %s not found", id)
	}
	isLocked := v.LockedBy != nil
	if isLocked != wantLocked {
		t.Errorf("volume %s locked=%v, want locked=%v", id, isLocked, wantLocked)
	}
}

func assertVolumeStoragePath(t *testing.T, store *fakeVolumeStore, id string, wantNonEmpty bool) {
	t.Helper()
	v := store.volumes[id]
	if v == nil {
		t.Fatalf("assertVolumeStoragePath: volume %s not found", id)
	}
	hasPath := v.StoragePath != nil && *v.StoragePath != ""
	if hasPath != wantNonEmpty {
		t.Errorf("volume %s hasStoragePath=%v, want %v (path=%v)", id, hasPath, wantNonEmpty, v.StoragePath)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// VOLUME_CREATE tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestVolumeCreate_HappyPath_TransitionsToAvailable(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-create-happy"
	store.volumes[volID] = newCreatingVolumeBlank(volID)

	h := newTestCreateVolumeHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_CREATE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
	assertVolumeLocked(t, store, volID, false)
	assertVolumeStoragePath(t, store, volID, true)
}

func TestVolumeCreate_AlreadyAvailable_IsNoOp(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-create-idem"
	store.volumes[volID] = newAvailableVolume(volID)
	versionBefore := store.volumes[volID].Version

	h := newTestCreateVolumeHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_CREATE")); err != nil {
		t.Errorf("Execute on available = %v, want nil (idempotent)", err)
	}
	if store.volumes[volID].Version != versionBefore {
		t.Errorf("version changed on no-op")
	}
}

func TestVolumeCreate_IllegalState_ReturnsError(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-create-badstate"
	vol := newAvailableVolume(volID)
	vol.Status = db.VolumeStatusDeleting
	store.volumes[volID] = vol

	h := newTestCreateVolumeHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_CREATE")); err == nil {
		t.Fatal("expected error for illegal state deleting, got nil")
	}
}

func TestVolumeCreate_LockConflict_ReturnsErrorStatusUnchanged(t *testing.T) {
	store := newFakeVolumeStore()
	store.lockFail = true
	const volID = "vol-create-lockfail"
	store.volumes[volID] = newCreatingVolumeBlank(volID)

	h := newTestCreateVolumeHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_CREATE")); err == nil {
		t.Fatal("expected error from lock conflict, got nil")
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusCreating)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeCreate_SetStoragePathFails_UnlocksToError(t *testing.T) {
	store := newFakeVolumeStore()
	store.setStoragePathFail = true
	const volID = "vol-create-storepath-fail"
	store.volumes[volID] = newCreatingVolumeBlank(volID)

	h := newTestCreateVolumeHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_CREATE")); err == nil {
		t.Fatal("expected error from SetVolumeStoragePath failure, got nil")
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusError)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeCreate_Reentrant_AlreadyLockedByThisJob_SkipsLock(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-create-reentrant"
	vol := newCreatingVolumeBlank(volID)
	jobID := "job-vol-001" // same as volumeJob() uses
	vol.LockedBy = &jobID
	vol.Version = 3
	store.volumes[volID] = vol

	h := newTestCreateVolumeHandler(store)
	// lockFail=true would catch an unexpected LockVolume call.
	store.lockFail = true
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_CREATE")); err != nil {
		t.Fatalf("re-entrant create: %v", err)
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
}

func TestVolumeCreate_NilVolumeID_ReturnsError(t *testing.T) {
	store := newFakeVolumeStore()
	job := &db.JobRow{ID: "job-nil-vol", VolumeID: nil, JobType: "VOLUME_CREATE"}

	h := newTestCreateVolumeHandler(store)
	if err := h.Execute(context.Background(), job); err == nil {
		t.Fatal("expected error for nil VolumeID, got nil")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// VOLUME_ATTACH tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestVolumeAttach_HappyPath_TransitionsToInUse(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-attach-happy"
	store.volumes[volID] = newAvailableVolume(volID)
	store.attachments[volID] = newAttachmentRow(volID, "inst-001")

	h := newTestAttachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_ATTACH")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertVolumeStatus(t, store, volID, db.VolumeStatusInUse)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeAttach_AlreadyInUse_IsNoOp(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-attach-idem"
	store.volumes[volID] = newInUseVolume(volID)
	versionBefore := store.volumes[volID].Version

	h := newTestAttachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_ATTACH")); err != nil {
		t.Errorf("Execute on in_use = %v, want nil (idempotent)", err)
	}
	// Version must not change — no mutation occurred.
	if store.volumes[volID].Version != versionBefore {
		t.Errorf("version changed on no-op: have %d want %d", store.volumes[volID].Version, versionBefore)
	}
}

func TestVolumeAttach_IllegalState_ReturnsError(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-attach-badstate"
	vol := newAvailableVolume(volID)
	vol.Status = db.VolumeStatusDeleting
	store.volumes[volID] = vol

	h := newTestAttachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_ATTACH")); err == nil {
		t.Fatal("expected error for illegal state, got nil")
	}
}

func TestVolumeAttach_LockConflict_ReturnsErrorStatusUnchanged(t *testing.T) {
	store := newFakeVolumeStore()
	store.lockFail = true
	const volID = "vol-attach-lockfail"
	store.volumes[volID] = newAvailableVolume(volID)

	h := newTestAttachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_ATTACH")); err == nil {
		t.Fatal("expected error from lock conflict, got nil")
	}
	// Lock failed before any status mutation — must remain available.
	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeAttach_MissingAttachment_RollsBackToAvailable(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-attach-noatt"
	store.volumes[volID] = newAvailableVolume(volID)
	// No attachment row seeded — simulates admission side-effect being absent.

	h := newTestAttachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_ATTACH")); err == nil {
		t.Fatal("expected error when attachment row missing, got nil")
	}
	// Worker must roll back to available.
	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeAttach_GetAttachmentFails_UnlocksToError(t *testing.T) {
	store := newFakeVolumeStore()
	store.getAttachFail = true
	const volID = "vol-attach-getfail"
	store.volumes[volID] = newAvailableVolume(volID)

	h := newTestAttachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_ATTACH")); err == nil {
		t.Fatal("expected error when GetActiveAttachmentByVolume fails, got nil")
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusError)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeAttach_Reentrant_AlreadyAttaching_CompletesToInUse(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-attach-reentrant"
	vol := newAvailableVolume(volID)
	vol.Status = db.VolumeStatusAttaching
	jobID := "job-vol-001"
	vol.LockedBy = &jobID
	vol.Version = 2
	store.volumes[volID] = vol
	store.attachments[volID] = newAttachmentRow(volID, "inst-001")

	h := newTestAttachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_ATTACH")); err != nil {
		t.Fatalf("re-entrant attach: %v", err)
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusInUse)
	assertVolumeLocked(t, store, volID, false)
}

// ═══════════════════════════════════════════════════════════════════════════════
// VOLUME_DETACH tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestVolumeDetach_HappyPath_TransitionsToAvailable(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-detach-happy"
	vol := newInUseVolume(volID)
	store.volumes[volID] = vol
	store.attachments[volID] = newAttachmentRow(volID, "inst-001")

	h := newTestDetachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DETACH")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
	assertVolumeLocked(t, store, volID, false)
	// Attachment must be closed (removed from active map).
	if _, active := store.attachments[volID]; active {
		t.Error("attachment still active after detach")
	}
}

func TestVolumeDetach_AlreadyAvailable_IsNoOp(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-detach-idem"
	store.volumes[volID] = newAvailableVolume(volID)
	versionBefore := store.volumes[volID].Version

	h := newTestDetachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DETACH")); err != nil {
		t.Errorf("Execute on available = %v, want nil (idempotent)", err)
	}
	if store.volumes[volID].Version != versionBefore {
		t.Errorf("version changed on no-op: have %d want %d", store.volumes[volID].Version, versionBefore)
	}
}

func TestVolumeDetach_IllegalState_ReturnsError(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-detach-badstate"
	vol := newAvailableVolume(volID)
	vol.Status = db.VolumeStatusDeleting
	store.volumes[volID] = vol

	h := newTestDetachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DETACH")); err == nil {
		t.Fatal("expected error for illegal state, got nil")
	}
}

func TestVolumeDetach_LockConflict_ReturnsErrorStatusUnchanged(t *testing.T) {
	store := newFakeVolumeStore()
	store.lockFail = true
	const volID = "vol-detach-lockfail"
	store.volumes[volID] = newInUseVolume(volID)

	h := newTestDetachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DETACH")); err == nil {
		t.Fatal("expected error from lock conflict, got nil")
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusInUse)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeDetach_CloseAttachmentFails_UnlocksToError(t *testing.T) {
	store := newFakeVolumeStore()
	store.closeAttachFail = true
	const volID = "vol-detach-closefail"
	store.volumes[volID] = newInUseVolume(volID)
	store.attachments[volID] = newAttachmentRow(volID, "inst-001")

	h := newTestDetachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DETACH")); err == nil {
		t.Fatal("expected error when CloseVolumeAttachment fails, got nil")
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusError)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeDetach_Reentrant_AlreadyDetaching_CompletesToAvailable(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-detach-reentrant"
	vol := newInUseVolume(volID)
	vol.Status = db.VolumeStatusDetaching
	jobID := "job-vol-001"
	vol.LockedBy = &jobID
	vol.Version = 4
	store.volumes[volID] = vol
	store.attachments[volID] = newAttachmentRow(volID, "inst-001")

	h := newTestDetachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DETACH")); err != nil {
		t.Fatalf("re-entrant detach: %v", err)
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeDetach_Reentrant_AttachmentAlreadyClosed_CompletesToAvailable(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-detach-reentrant-closed"
	vol := newInUseVolume(volID)
	vol.Status = db.VolumeStatusDetaching
	jobID := "job-vol-001"
	vol.LockedBy = &jobID
	store.volumes[volID] = vol
	// No attachment row — already closed in prior partial run.

	h := newTestDetachHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DETACH")); err != nil {
		t.Fatalf("re-entrant detach (closed attachment): %v", err)
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
}

// ═══════════════════════════════════════════════════════════════════════════════
// VOLUME_DELETE tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestVolumeDelete_HappyPath_TransitionsToDeleted(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-delete-happy"
	store.volumes[volID] = newAvailableVolume(volID)

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusDeleted)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeDelete_AlreadyDeleted_IsNoOp(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-delete-idem"
	vol := newAvailableVolume(volID)
	now := time.Now()
	vol.Status = db.VolumeStatusDeleted
	vol.DeletedAt = &now
	store.volumes[volID] = vol
	versionBefore := vol.Version

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err != nil {
		t.Errorf("Execute on deleted = %v, want nil (idempotent)", err)
	}
	if store.volumes[volID].Version != versionBefore {
		t.Errorf("version changed on no-op")
	}
}

func TestVolumeDelete_VolumeNotFound_IsNoOp(t *testing.T) {
	store := newFakeVolumeStore()
	// Nothing seeded — volume row doesn't exist; GetVolumeByID returns nil,nil.
	volID := "vol-delete-ghost"

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err != nil {
		t.Errorf("Execute on missing volume = %v, want nil (idempotent no-op)", err)
	}
}

func TestVolumeDelete_InUse_ReturnsError(t *testing.T) {
	// VOL-SM-1: in_use volumes must not be deleted.
	store := newFakeVolumeStore()
	const volID = "vol-delete-inuse"
	store.volumes[volID] = newInUseVolume(volID)

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err == nil {
		t.Fatal("expected error when deleting in_use volume, got nil")
	}
	// Status must remain in_use — no mutation.
	assertVolumeStatus(t, store, volID, db.VolumeStatusInUse)
}

func TestVolumeDelete_IllegalState_ReturnsError(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-delete-badstate"
	vol := newAvailableVolume(volID)
	vol.Status = db.VolumeStatusCreating
	store.volumes[volID] = vol

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err == nil {
		t.Fatal("expected error for illegal state creating, got nil")
	}
}

func TestVolumeDelete_LockConflict_ReturnsErrorStatusUnchanged(t *testing.T) {
	store := newFakeVolumeStore()
	store.lockFail = true
	const volID = "vol-delete-lockfail"
	store.volumes[volID] = newAvailableVolume(volID)

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err == nil {
		t.Fatal("expected error from lock conflict, got nil")
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeDelete_SoftDeleteFails_UnlocksToError(t *testing.T) {
	store := newFakeVolumeStore()
	store.softDeleteFail = true
	const volID = "vol-delete-softfail"
	store.volumes[volID] = newAvailableVolume(volID)

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err == nil {
		t.Fatal("expected error, got nil")
	}
	// Volume must be in error — inspectable failure state with lock cleared.
	assertVolumeStatus(t, store, volID, db.VolumeStatusError)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeDelete_Reentrant_AlreadyDeleting_CompletesToDeleted(t *testing.T) {
	store := newFakeVolumeStore()
	const volID = "vol-delete-reentrant"
	vol := newAvailableVolume(volID)
	vol.Status = db.VolumeStatusDeleting
	jobID := "job-vol-001"
	vol.LockedBy = &jobID
	vol.Version = 5
	store.volumes[volID] = vol

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), volumeJob(volID, "VOLUME_DELETE")); err != nil {
		t.Fatalf("re-entrant delete: %v", err)
	}
	assertVolumeStatus(t, store, volID, db.VolumeStatusDeleted)
	assertVolumeLocked(t, store, volID, false)
}

func TestVolumeDelete_NilVolumeID_ReturnsError(t *testing.T) {
	store := newFakeVolumeStore()
	job := &db.JobRow{ID: "job-nil-vol", VolumeID: nil, JobType: "VOLUME_DELETE"}

	h := newTestVolumeDeleteHandler(store)
	if err := h.Execute(context.Background(), job); err == nil {
		t.Fatal("expected error for nil VolumeID, got nil")
	}
}
