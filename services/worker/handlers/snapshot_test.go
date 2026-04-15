package handlers

// snapshot_test.go — Unit tests for SnapshotCreateHandler, SnapshotDeleteHandler,
// VolumeRestoreHandler.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.4 (state machine), §2.5 (transitions),
//         §2.9 (invariants SNAP-I-1, SNAP-I-3),
//         vm-15-02__skill__snapshot-clone-restore-retention-model.md.
//         VM-P2B-S2.
//         VM-P2B-S3: extended fakeSnapshotStore with CountVolumesBySourceSnapshot;
//           added SNAP-I-3 enforcement test for SnapshotDeleteHandler.
//
// All tests use in-memory fakeSnapshotStore. No real DB required.
//
// Test matrix:
//   SNAPSHOT_CREATE
//     - success: pending → creating → available, storage_path set
//     - idempotent: already available → no-op
//     - illegal state (deleting) → error
//     - lock conflict → error, status remains pending
//     - re-entrant: already creating → skip lock, complete to available
//     - nil SnapshotID → error
//   SNAPSHOT_DELETE
//     - success: available → deleting → deleted
//     - success from error: error → deleting → deleted
//     - idempotent: already deleted → no-op
//     - idempotent: not found → no-op
//     - illegal state (creating) → error
//     - lock conflict → error, status remains available
//     - re-entrant: already deleting → skip lock, complete to deleted
//     - SNAP-I-3: active restored volumes block delete → error
//     - nil SnapshotID → error
//   VOLUME_RESTORE
//     - success: snapshot available + volume creating → volume available, storage_path set
//     - idempotent: volume already available → no-op
//     - snapshot not available → error
//     - snapshot not found → error
//     - volume not found → error
//     - volume wrong state (e.g. in_use) → error
//     - nil SnapshotID → error
//     - nil VolumeID → error
//   Interface compliance: fakeSnapshotStore satisfies SnapshotStore (compile check)

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// snapTestLog returns a discard logger for use in snapshot handler tests.
// Uses a package-private name to avoid collision with testLog defined in
// other test files in this package.
func snapTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ── fakeSnapshotStore ─────────────────────────────────────────────────────────

type fakeSnapshotStore struct {
	mu        sync.Mutex
	snapshots map[string]*db.SnapshotRow
	volumes   map[string]*db.VolumeRow

	// restoredVolumeCounts: snapshotID → number of non-deleted restored volumes.
	// Used by CountVolumesBySourceSnapshot (SNAP-I-3).
	restoredVolumeCounts map[string]int

	// Fault injection.
	lockFail         bool
	updateStatusFail bool
	markAvailFail    bool
	softDeleteFail   bool
}

func newFakeSnapshotStore() *fakeSnapshotStore {
	return &fakeSnapshotStore{
		snapshots:            make(map[string]*db.SnapshotRow),
		volumes:              make(map[string]*db.VolumeRow),
		restoredVolumeCounts: make(map[string]int),
	}
}

func (s *fakeSnapshotStore) GetSnapshotByID(_ context.Context, id string) (*db.SnapshotRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.snapshots[id]
	if !ok {
		return nil, nil
	}
	cp := *v
	return &cp, nil
}

func (s *fakeSnapshotStore) LockSnapshot(_ context.Context, id, jobID, expectedStatus string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFail {
		return errors.New("fakeSnapshotStore: LockSnapshot failure injected")
	}
	v, ok := s.snapshots[id]
	if !ok {
		return fmt.Errorf("LockSnapshot: snapshot %s not found", id)
	}
	if v.Status != expectedStatus {
		return fmt.Errorf("LockSnapshot: status mismatch have %q expected %q", v.Status, expectedStatus)
	}
	if v.Version != version {
		return fmt.Errorf("LockSnapshot: version mismatch have %d expected %d", v.Version, version)
	}
	if v.LockedBy != nil {
		return fmt.Errorf("LockSnapshot: snapshot %s already locked by %s", id, *v.LockedBy)
	}
	v.LockedBy = &jobID
	v.Version++
	return nil
}

func (s *fakeSnapshotStore) UnlockSnapshot(_ context.Context, id, newStatus string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.snapshots[id]
	if !ok {
		return fmt.Errorf("UnlockSnapshot: snapshot %s not found", id)
	}
	v.LockedBy = nil
	v.Status = newStatus
	v.Version++
	return nil
}

func (s *fakeSnapshotStore) UpdateSnapshotStatus(_ context.Context, id, expectedStatus, newStatus string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateStatusFail {
		return errors.New("fakeSnapshotStore: UpdateSnapshotStatus failure injected")
	}
	v, ok := s.snapshots[id]
	if !ok {
		return fmt.Errorf("UpdateSnapshotStatus: snapshot %s not found", id)
	}
	if v.Status != expectedStatus {
		return fmt.Errorf("UpdateSnapshotStatus: status mismatch have %q expected %q", v.Status, expectedStatus)
	}
	if v.Version != version {
		return fmt.Errorf("UpdateSnapshotStatus: version mismatch have %d expected %d", v.Version, version)
	}
	v.Status = newStatus
	v.Version++
	return nil
}

func (s *fakeSnapshotStore) MarkSnapshotAvailable(_ context.Context, id, storagePath string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markAvailFail {
		return errors.New("fakeSnapshotStore: MarkSnapshotAvailable failure injected")
	}
	v, ok := s.snapshots[id]
	if !ok {
		return fmt.Errorf("MarkSnapshotAvailable: snapshot %s not found", id)
	}
	if v.Version != version {
		return fmt.Errorf("MarkSnapshotAvailable: version mismatch have %d expected %d", v.Version, version)
	}
	now := time.Now()
	v.Status = db.SnapshotStatusAvailable
	v.StoragePath = &storagePath
	v.ProgressPercent = 100
	v.CompletedAt = &now
	v.LockedBy = nil
	v.Version++
	return nil
}

func (s *fakeSnapshotStore) SoftDeleteSnapshot(_ context.Context, id string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.softDeleteFail {
		return errors.New("fakeSnapshotStore: SoftDeleteSnapshot failure injected")
	}
	v, ok := s.snapshots[id]
	if !ok {
		return fmt.Errorf("SoftDeleteSnapshot: snapshot %s not found", id)
	}
	if v.Version != version {
		return fmt.Errorf("SoftDeleteSnapshot: version mismatch have %d expected %d", v.Version, version)
	}
	now := time.Now()
	v.Status = db.SnapshotStatusDeleted
	v.DeletedAt = &now
	v.LockedBy = nil
	v.Version++
	return nil
}

func (s *fakeSnapshotStore) GetVolumeByID(_ context.Context, id string) (*db.VolumeRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[id]
	if !ok {
		return nil, nil
	}
	cp := *v
	return &cp, nil
}

func (s *fakeSnapshotStore) UnlockVolume(_ context.Context, id, newStatus string) error {
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

// SetVolumeStoragePath records the storage_path on the volume.
// VM-P2B-S3: required by SnapshotStore (used by VOLUME_RESTORE handler).
func (s *fakeSnapshotStore) SetVolumeStoragePath(_ context.Context, id, storagePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.volumes[id]
	if !ok {
		return fmt.Errorf("SetVolumeStoragePath: volume %s not found", id)
	}
	v.StoragePath = &storagePath
	return nil
}

// CountVolumesBySourceSnapshot returns the injected count of restored volumes.
// VM-P2B-S3: required by SnapshotStore interface for SNAP-I-3 enforcement.
func (s *fakeSnapshotStore) CountVolumesBySourceSnapshot(_ context.Context, snapshotID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoredVolumeCounts[snapshotID], nil
}

// compile-time interface check
var _ SnapshotStore = (*fakeSnapshotStore)(nil)

// ── Fixtures ──────────────────────────────────────────────────────────────────

func newPendingSnapshot(id string) *db.SnapshotRow {
	return &db.SnapshotRow{
		ID:               id,
		OwnerPrincipalID: "princ_test",
		DisplayName:      "snap-" + id,
		Region:           "us-east-1",
		SizeGB:           50,
		Status:           db.SnapshotStatusPending,
		ProgressPercent:  0,
		Version:          0,
	}
}

func newAvailableSnapshot(id string) *db.SnapshotRow {
	s := newPendingSnapshot(id)
	path := "/snapshots/" + id + "/data"
	s.Status = db.SnapshotStatusAvailable
	s.StoragePath = &path
	s.ProgressPercent = 100
	return s
}

func newCreatingVolume(id, snapID string) *db.VolumeRow {
	return &db.VolumeRow{
		ID:               id,
		OwnerPrincipalID: "princ_test",
		DisplayName:      "vol-" + id,
		Region:           "us-east-1",
		AvailabilityZone: "us-east-1a",
		SizeGB:           50,
		Origin:           "snapshot",
		SourceSnapshotID: &snapID,
		Status:           db.VolumeStatusCreating,
		Version:          0,
	}
}

func snapJob(snapID, jobType string) *db.JobRow {
	return &db.JobRow{
		ID:         "job-snap-001",
		SnapshotID: &snapID,
		JobType:    jobType,
	}
}

func restoreJob(snapID, volID string) *db.JobRow {
	return &db.JobRow{
		ID:         "job-restore-001",
		SnapshotID: &snapID,
		VolumeID:   &volID,
		JobType:    "VOLUME_RESTORE",
	}
}

func newTestSnapshotCreateHandler(store *fakeSnapshotStore) *SnapshotCreateHandler {
	return NewSnapshotCreateHandler(&SnapshotDeps{Store: store}, snapTestLog())
}
func newTestSnapshotDeleteHandler(store *fakeSnapshotStore) *SnapshotDeleteHandler {
	return NewSnapshotDeleteHandler(&SnapshotDeps{Store: store}, snapTestLog())
}
func newTestVolumeRestoreHandler(store *fakeSnapshotStore) *VolumeRestoreHandler {
	return NewVolumeRestoreHandler(&SnapshotDeps{Store: store}, snapTestLog())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func assertSnapshotStatus(t *testing.T, store *fakeSnapshotStore, id, wantStatus string) {
	t.Helper()
	s := store.snapshots[id]
	if s == nil {
		t.Fatalf("assertSnapshotStatus: snapshot %s not found", id)
	}
	if s.Status != wantStatus {
		t.Errorf("snapshot %s status: got %q want %q", id, s.Status, wantStatus)
	}
}

func assertSnapshotLocked(t *testing.T, store *fakeSnapshotStore, id string, wantLocked bool) {
	t.Helper()
	s := store.snapshots[id]
	if s == nil {
		t.Fatalf("assertSnapshotLocked: snapshot %s not found", id)
	}
	isLocked := s.LockedBy != nil
	if isLocked != wantLocked {
		t.Errorf("snapshot %s locked=%v, want locked=%v", id, isLocked, wantLocked)
	}
}

func assertSnapshotVolumeStatus(t *testing.T, store *fakeSnapshotStore, id, wantStatus string) {
	t.Helper()
	v := store.volumes[id]
	if v == nil {
		t.Fatalf("assertVolumeStatus: volume %s not found", id)
	}
	if v.Status != wantStatus {
		t.Errorf("volume %s status: got %q want %q", id, v.Status, wantStatus)
	}
}

func assertSnapshotVolumeStoragePath(t *testing.T, store *fakeSnapshotStore, id string, wantNonEmpty bool) {
	t.Helper()
	v := store.volumes[id]
	if v == nil {
		t.Fatalf("assertSnapshotVolumeStoragePath: volume %s not found", id)
	}
	hasPath := v.StoragePath != nil && *v.StoragePath != ""
	if hasPath != wantNonEmpty {
		t.Errorf("volume %s hasStoragePath=%v, want %v", id, hasPath, wantNonEmpty)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// SNAPSHOT_CREATE tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestSnapshotCreate_HappyPath_TransitionsToAvailable(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-create-happy"
	store.snapshots[snapID] = newPendingSnapshot(snapID)

	h := newTestSnapshotCreateHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_CREATE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusAvailable)
	assertSnapshotLocked(t, store, snapID, false)

	s := store.snapshots[snapID]
	if s.StoragePath == nil || *s.StoragePath == "" {
		t.Error("want non-empty storage_path after create")
	}
	if s.ProgressPercent != 100 {
		t.Errorf("want progress_percent=100, got %d", s.ProgressPercent)
	}
	if s.CompletedAt == nil {
		t.Error("want completed_at set after create")
	}
}

func TestSnapshotCreate_AlreadyAvailable_IsNoOp(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-create-idem"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)
	versionBefore := store.snapshots[snapID].Version

	h := newTestSnapshotCreateHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_CREATE")); err != nil {
		t.Errorf("Execute on available = %v, want nil (idempotent)", err)
	}
	if store.snapshots[snapID].Version != versionBefore {
		t.Errorf("version changed on no-op")
	}
}

func TestSnapshotCreate_IllegalState_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-create-bad"
	s := newPendingSnapshot(snapID)
	s.Status = db.SnapshotStatusDeleting
	store.snapshots[snapID] = s

	h := newTestSnapshotCreateHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_CREATE")); err == nil {
		t.Fatal("expected error for illegal state deleting, got nil")
	}
}

func TestSnapshotCreate_LockConflict_ReturnsErrorStatusUnchanged(t *testing.T) {
	store := newFakeSnapshotStore()
	store.lockFail = true
	const snapID = "snap-create-lockfail"
	store.snapshots[snapID] = newPendingSnapshot(snapID)

	h := newTestSnapshotCreateHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_CREATE")); err == nil {
		t.Fatal("expected error from lock conflict, got nil")
	}
	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusPending)
	assertSnapshotLocked(t, store, snapID, false)
}

func TestSnapshotCreate_Reentrant_AlreadyCreating_CompletesToAvailable(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-create-reentrant"
	s := newPendingSnapshot(snapID)
	s.Status = db.SnapshotStatusCreating
	jobID := "job-snap-001"
	s.LockedBy = &jobID
	s.Version = 2
	store.snapshots[snapID] = s

	h := newTestSnapshotCreateHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_CREATE")); err != nil {
		t.Fatalf("re-entrant create: %v", err)
	}
	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusAvailable)
}

func TestSnapshotCreate_NilSnapshotID_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	job := &db.JobRow{ID: "job-nil-snap", SnapshotID: nil, JobType: "SNAPSHOT_CREATE"}

	h := newTestSnapshotCreateHandler(store)
	if err := h.Execute(context.Background(), job); err == nil {
		t.Fatal("expected error for nil SnapshotID, got nil")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// SNAPSHOT_DELETE tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestSnapshotDelete_HappyPath_TransitionsToDeleted(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-delete-happy"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_DELETE")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusDeleted)
	assertSnapshotLocked(t, store, snapID, false)
}

func TestSnapshotDelete_FromErrorState_TransitionsToDeleted(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-delete-fromerr"
	s := newAvailableSnapshot(snapID)
	s.Status = db.SnapshotStatusError
	store.snapshots[snapID] = s

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_DELETE")); err != nil {
		t.Fatalf("Execute from error state: %v", err)
	}
	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusDeleted)
}

func TestSnapshotDelete_AlreadyDeleted_IsNoOp(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-delete-idem"
	s := newAvailableSnapshot(snapID)
	now := time.Now()
	s.Status = db.SnapshotStatusDeleted
	s.DeletedAt = &now
	store.snapshots[snapID] = s
	versionBefore := s.Version

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_DELETE")); err != nil {
		t.Errorf("Execute on deleted = %v, want nil (idempotent)", err)
	}
	if store.snapshots[snapID].Version != versionBefore {
		t.Errorf("version changed on no-op")
	}
}

func TestSnapshotDelete_NotFound_IsNoOp(t *testing.T) {
	store := newFakeSnapshotStore()

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob("snap-ghost", "SNAPSHOT_DELETE")); err != nil {
		t.Errorf("Execute on missing snapshot = %v, want nil (idempotent)", err)
	}
}

func TestSnapshotDelete_IllegalState_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-delete-bad"
	s := newPendingSnapshot(snapID)
	s.Status = db.SnapshotStatusCreating
	store.snapshots[snapID] = s

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_DELETE")); err == nil {
		t.Fatal("expected error for illegal state creating, got nil")
	}
}

func TestSnapshotDelete_LockConflict_ReturnsErrorStatusUnchanged(t *testing.T) {
	store := newFakeSnapshotStore()
	store.lockFail = true
	const snapID = "snap-delete-lockfail"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_DELETE")); err == nil {
		t.Fatal("expected error from lock conflict, got nil")
	}
	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusAvailable)
	assertSnapshotLocked(t, store, snapID, false)
}

func TestSnapshotDelete_Reentrant_AlreadyDeleting_CompletesToDeleted(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-delete-reentrant"
	s := newAvailableSnapshot(snapID)
	s.Status = db.SnapshotStatusDeleting
	jobID := "job-snap-001"
	s.LockedBy = &jobID
	s.Version = 3
	store.snapshots[snapID] = s

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_DELETE")); err != nil {
		t.Fatalf("re-entrant delete: %v", err)
	}
	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusDeleted)
	assertSnapshotLocked(t, store, snapID, false)
}

// TestSnapshotDelete_SNAPI3_ActiveRestoredVolumes_ReturnsError verifies that
// SNAP-I-3 is enforced: cannot delete a snapshot that has non-deleted volumes
// restored from it.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3. VM-P2B-S3.
func TestSnapshotDelete_SNAPI3_ActiveRestoredVolumes_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-delete-snapi3"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)
	// Inject restored volume count — simulates 1 non-deleted volume from this snap.
	store.restoredVolumeCounts[snapID] = 1

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), snapJob(snapID, "SNAPSHOT_DELETE")); err == nil {
		t.Fatal("expected error for SNAP-I-3 violation, got nil")
	}
	// Snapshot must remain available — no mutation should have occurred.
	assertSnapshotStatus(t, store, snapID, db.SnapshotStatusAvailable)
	assertSnapshotLocked(t, store, snapID, false)
}

func TestSnapshotDelete_NilSnapshotID_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	job := &db.JobRow{ID: "job-nil-snap", SnapshotID: nil, JobType: "SNAPSHOT_DELETE"}

	h := newTestSnapshotDeleteHandler(store)
	if err := h.Execute(context.Background(), job); err == nil {
		t.Fatal("expected error for nil SnapshotID, got nil")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// VOLUME_RESTORE tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestVolumeRestore_HappyPath_VolumeBecomesAvailable(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-restore-happy"
	const volID = "vol-restore-happy"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)
	store.volumes[volID] = newCreatingVolume(volID, snapID)

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), restoreJob(snapID, volID)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertSnapshotVolumeStatus(t, store, volID, db.VolumeStatusAvailable)
	// VM-P2B-S3: storage_path must be set.
	assertSnapshotVolumeStoragePath(t, store, volID, true)
}

func TestVolumeRestore_VolumeAlreadyAvailable_IsNoOp(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-restore-idem"
	const volID = "vol-restore-idem"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)
	v := newCreatingVolume(volID, snapID)
	v.Status = db.VolumeStatusAvailable
	store.volumes[volID] = v
	versionBefore := v.Version

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), restoreJob(snapID, volID)); err != nil {
		t.Errorf("Execute on available volume = %v, want nil (idempotent)", err)
	}
	if store.volumes[volID].Version != versionBefore {
		t.Errorf("version changed on no-op")
	}
}

func TestVolumeRestore_SnapshotNotAvailable_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-restore-notavail"
	const volID = "vol-restore-notavail"
	s := newPendingSnapshot(snapID)
	s.Status = db.SnapshotStatusCreating
	store.snapshots[snapID] = s
	store.volumes[volID] = newCreatingVolume(volID, snapID)

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), restoreJob(snapID, volID)); err == nil {
		t.Fatal("expected error when snapshot not available, got nil")
	}
}

func TestVolumeRestore_SnapshotNotFound_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const volID = "vol-restore-nosnap"

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), restoreJob("snap-ghost", volID)); err == nil {
		t.Fatal("expected error when snapshot not found, got nil")
	}
}

func TestVolumeRestore_VolumeNotFound_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-restore-novol"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)
	// volume not seeded

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), restoreJob(snapID, "vol-ghost")); err == nil {
		t.Fatal("expected error when volume not found, got nil")
	}
}

func TestVolumeRestore_VolumeWrongState_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-restore-wrongstate"
	const volID = "vol-restore-wrongstate"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)
	v := newCreatingVolume(volID, snapID)
	v.Status = db.VolumeStatusInUse
	store.volumes[volID] = v

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), restoreJob(snapID, volID)); err == nil {
		t.Fatal("expected error for volume in wrong state, got nil")
	}
}

func TestVolumeRestore_NilSnapshotID_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	job := &db.JobRow{ID: "job-nil", SnapshotID: nil, VolumeID: strPtr("vol-x"), JobType: "VOLUME_RESTORE"}

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), job); err == nil {
		t.Fatal("expected error for nil SnapshotID, got nil")
	}
}

func TestVolumeRestore_NilVolumeID_ReturnsError(t *testing.T) {
	store := newFakeSnapshotStore()
	const snapID = "snap-restore-nilvol"
	store.snapshots[snapID] = newAvailableSnapshot(snapID)
	snapIDVar := snapID
	job := &db.JobRow{ID: "job-nil-vol", SnapshotID: &snapIDVar, VolumeID: nil, JobType: "VOLUME_RESTORE"}

	h := newTestVolumeRestoreHandler(store)
	if err := h.Execute(context.Background(), job); err == nil {
		t.Fatal("expected error for nil VolumeID, got nil")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }
