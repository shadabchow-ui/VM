package main

// root_disk_repo_test.go — M10 Slice 2: Volume Foundation tests.
//
// Tests for RootDiskRow model and repository operations.
// Uses the memPool testing infrastructure from instance_handlers_test.go.
//
// Test coverage:
//   - RootDiskRow CRUD operations
//   - delete_on_termination=true → disk deleted with instance
//   - delete_on_termination=false → disk detached (status=DETACHED, instance_id=NULL)
//   - Status transitions (CREATING → ATTACHED → DETACHED)
//   - ListDetachedRootDisks for Phase 2 volume service
//
// Source: 06-01-root-disk-model-and-persistence-semantics.md,
//         P2_VOLUME_MODEL.md §1, P2_MIGRATION_COMPATIBILITY_RULES.md §7.

import (
	"context"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── RootDiskRepo wrapper for testing ────────────────────────────────────────

// testRootDiskRepo wraps memPool to provide repo-like operations for testing.
// This allows unit testing the root disk logic without a real database.
type testRootDiskRepo struct {
	mem *memPool
}

func (r *testRootDiskRepo) CreateRootDisk(ctx context.Context, row *db.RootDiskRow) error {
	r.mem.seedRootDisk(row)
	return nil
}

func (r *testRootDiskRepo) GetRootDiskByID(ctx context.Context, diskID string) (*db.RootDiskRow, error) {
	disk, ok := r.mem.rootDisks[diskID]
	if !ok {
		return nil, nil
	}
	return disk, nil
}

func (r *testRootDiskRepo) GetRootDiskByInstanceID(ctx context.Context, instanceID string) (*db.RootDiskRow, error) {
	for _, disk := range r.mem.rootDisks {
		if disk.InstanceID != nil && *disk.InstanceID == instanceID {
			return disk, nil
		}
	}
	return nil, nil
}

func (r *testRootDiskRepo) UpdateRootDiskStatus(ctx context.Context, diskID, status string) error {
	disk, ok := r.mem.rootDisks[diskID]
	if !ok {
		return nil
	}
	disk.Status = status
	return nil
}

func (r *testRootDiskRepo) DetachRootDisk(ctx context.Context, diskID string) error {
	disk, ok := r.mem.rootDisks[diskID]
	if !ok {
		return nil
	}
	disk.InstanceID = nil
	disk.Status = db.RootDiskStatusDetached
	return nil
}

func (r *testRootDiskRepo) DeleteRootDisk(ctx context.Context, diskID string) error {
	delete(r.mem.rootDisks, diskID)
	return nil
}

func (r *testRootDiskRepo) ListDetachedRootDisks(ctx context.Context, limit int) ([]*db.RootDiskRow, error) {
	var out []*db.RootDiskRow
	for _, disk := range r.mem.rootDisks {
		if disk.Status == db.RootDiskStatusDetached {
			out = append(out, disk)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestRootDisk_CreateAndGet(t *testing.T) {
	mem := newMemPool()
	repo := &testRootDiskRepo{mem: mem}

	instID := "inst_test001"
	disk := &db.RootDiskRow{
		DiskID:              "disk_abc123",
		InstanceID:          &instID,
		SourceImageID:       "img_ubuntu2204",
		StoragePoolID:       "pool_nfs01",
		StoragePath:         "nfs://filer/vol/disk_abc123.qcow2",
		SizeGB:              50,
		DeleteOnTermination: true,
		Status:              db.RootDiskStatusCreating,
	}

	err := repo.CreateRootDisk(context.Background(), disk)
	if err != nil {
		t.Fatalf("CreateRootDisk: %v", err)
	}

	// Verify GetRootDiskByID
	got, err := repo.GetRootDiskByID(context.Background(), "disk_abc123")
	if err != nil {
		t.Fatalf("GetRootDiskByID: %v", err)
	}
	if got == nil {
		t.Fatal("GetRootDiskByID: expected disk, got nil")
	}
	if got.DiskID != "disk_abc123" {
		t.Errorf("want disk_id=disk_abc123, got %q", got.DiskID)
	}
	if got.Status != db.RootDiskStatusCreating {
		t.Errorf("want status=CREATING, got %q", got.Status)
	}

	// Verify GetRootDiskByInstanceID
	got2, err := repo.GetRootDiskByInstanceID(context.Background(), "inst_test001")
	if err != nil {
		t.Fatalf("GetRootDiskByInstanceID: %v", err)
	}
	if got2 == nil {
		t.Fatal("GetRootDiskByInstanceID: expected disk, got nil")
	}
	if got2.DiskID != "disk_abc123" {
		t.Errorf("want disk_id=disk_abc123, got %q", got2.DiskID)
	}
}

func TestRootDisk_StatusTransitions(t *testing.T) {
	mem := newMemPool()
	repo := &testRootDiskRepo{mem: mem}

	instID := "inst_lifecycle"
	disk := &db.RootDiskRow{
		DiskID:              "disk_lifecycle",
		InstanceID:          &instID,
		SourceImageID:       "img_test",
		StoragePoolID:       "pool_01",
		StoragePath:         "nfs://filer/vol/disk_lifecycle.qcow2",
		SizeGB:              80,
		DeleteOnTermination: true,
		Status:              db.RootDiskStatusCreating,
	}
	_ = repo.CreateRootDisk(context.Background(), disk)

	// CREATING → ATTACHED
	err := repo.UpdateRootDiskStatus(context.Background(), "disk_lifecycle", db.RootDiskStatusAttached)
	if err != nil {
		t.Fatalf("UpdateRootDiskStatus to ATTACHED: %v", err)
	}

	got, _ := repo.GetRootDiskByID(context.Background(), "disk_lifecycle")
	if got.Status != db.RootDiskStatusAttached {
		t.Errorf("want status=ATTACHED, got %q", got.Status)
	}
}

func TestRootDisk_DeleteOnTerminationTrue(t *testing.T) {
	// When delete_on_termination=true, disk is deleted with instance.
	mem := newMemPool()
	repo := &testRootDiskRepo{mem: mem}

	instID := "inst_delete_true"
	disk := &db.RootDiskRow{
		DiskID:              "disk_ephemeral",
		InstanceID:          &instID,
		SourceImageID:       "img_test",
		StoragePoolID:       "pool_01",
		StoragePath:         "nfs://filer/vol/disk_ephemeral.qcow2",
		SizeGB:              50,
		DeleteOnTermination: true, // <- delete with instance
		Status:              db.RootDiskStatusAttached,
	}
	_ = repo.CreateRootDisk(context.Background(), disk)

	// Simulate instance deletion: delete the disk
	err := repo.DeleteRootDisk(context.Background(), "disk_ephemeral")
	if err != nil {
		t.Fatalf("DeleteRootDisk: %v", err)
	}

	// Verify disk is gone
	got, _ := repo.GetRootDiskByID(context.Background(), "disk_ephemeral")
	if got != nil {
		t.Error("disk should be deleted when delete_on_termination=true")
	}
}

func TestRootDisk_DeleteOnTerminationFalse(t *testing.T) {
	// When delete_on_termination=false, disk is detached, not deleted.
	// This is the Phase 2 persistent volume entry point.
	// Source: 06-01-root-disk-model-and-persistence-semantics.md, P2_VOLUME_MODEL.md §1.
	mem := newMemPool()
	repo := &testRootDiskRepo{mem: mem}

	instID := "inst_delete_false"
	disk := &db.RootDiskRow{
		DiskID:              "disk_persistent",
		InstanceID:          &instID,
		SourceImageID:       "img_test",
		StoragePoolID:       "pool_01",
		StoragePath:         "nfs://filer/vol/disk_persistent.qcow2",
		SizeGB:              100,
		DeleteOnTermination: false, // <- retain as detached volume
		Status:              db.RootDiskStatusAttached,
	}
	_ = repo.CreateRootDisk(context.Background(), disk)

	// Simulate instance deletion: detach the disk instead of delete
	err := repo.DetachRootDisk(context.Background(), "disk_persistent")
	if err != nil {
		t.Fatalf("DetachRootDisk: %v", err)
	}

	// Verify disk still exists with DETACHED status and no instance_id
	got, _ := repo.GetRootDiskByID(context.Background(), "disk_persistent")
	if got == nil {
		t.Fatal("disk should exist when delete_on_termination=false")
	}
	if got.Status != db.RootDiskStatusDetached {
		t.Errorf("want status=DETACHED, got %q", got.Status)
	}
	if got.InstanceID != nil {
		t.Error("instance_id should be NULL after detach")
	}
}

func TestRootDisk_ListDetached(t *testing.T) {
	// Phase 2 volume service uses ListDetachedRootDisks to surface detached disks as volumes.
	// Source: P2_VOLUME_MODEL.md §1.
	mem := newMemPool()
	repo := &testRootDiskRepo{mem: mem}

	// Create some disks in various states
	inst1 := "inst_1"
	inst2 := "inst_2"

	// Attached disk (should NOT appear in list)
	mem.seedRootDisk(&db.RootDiskRow{
		DiskID:     "disk_attached",
		InstanceID: &inst1,
		Status:     db.RootDiskStatusAttached,
	})

	// Creating disk (should NOT appear in list)
	mem.seedRootDisk(&db.RootDiskRow{
		DiskID:     "disk_creating",
		InstanceID: &inst2,
		Status:     db.RootDiskStatusCreating,
	})

	// Detached disks (SHOULD appear in list)
	mem.seedRootDisk(&db.RootDiskRow{
		DiskID:     "disk_detached_1",
		InstanceID: nil,
		Status:     db.RootDiskStatusDetached,
	})
	mem.seedRootDisk(&db.RootDiskRow{
		DiskID:     "disk_detached_2",
		InstanceID: nil,
		Status:     db.RootDiskStatusDetached,
	})

	// List detached disks
	list, err := repo.ListDetachedRootDisks(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListDetachedRootDisks: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("want 2 detached disks, got %d", len(list))
	}

	// Verify all returned disks are DETACHED
	for _, d := range list {
		if d.Status != db.RootDiskStatusDetached {
			t.Errorf("disk %s: want status=DETACHED, got %q", d.DiskID, d.Status)
		}
	}
}

func TestRootDisk_GetByID_NotFound(t *testing.T) {
	mem := newMemPool()
	repo := &testRootDiskRepo{mem: mem}

	got, err := repo.GetRootDiskByID(context.Background(), "disk_nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent disk")
	}
}

func TestRootDisk_GetByInstanceID_NotFound(t *testing.T) {
	mem := newMemPool()
	repo := &testRootDiskRepo{mem: mem}

	got, err := repo.GetRootDiskByInstanceID(context.Background(), "inst_nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent instance")
	}
}

func TestRootDisk_StatusConstants(t *testing.T) {
	// Verify status constants match the contract.
	// Source: INSTANCE_MODEL_V1.md §8.
	if db.RootDiskStatusCreating != "CREATING" {
		t.Errorf("want CREATING, got %q", db.RootDiskStatusCreating)
	}
	if db.RootDiskStatusAttached != "ATTACHED" {
		t.Errorf("want ATTACHED, got %q", db.RootDiskStatusAttached)
	}
	if db.RootDiskStatusDetached != "DETACHED" {
		t.Errorf("want DETACHED, got %q", db.RootDiskStatusDetached)
	}
}

func TestRootDisk_InstanceDeletion_WithDeleteOnTermination(t *testing.T) {
	// Integration-style test: simulate full instance deletion scenarios.
	// This tests the logic flow, not the actual delete handlers.

	t.Run("delete_on_termination=true", func(t *testing.T) {
		mem := newMemPool()
		repo := &testRootDiskRepo{mem: mem}

		instID := "inst_to_delete_1"
		disk := &db.RootDiskRow{
			DiskID:              "disk_001",
			InstanceID:          &instID,
			SourceImageID:       "img_test",
			StoragePoolID:       "pool_01",
			StoragePath:         "nfs://filer/vol/disk_001.qcow2",
			SizeGB:              50,
			DeleteOnTermination: true,
			Status:              db.RootDiskStatusAttached,
		}
		_ = repo.CreateRootDisk(context.Background(), disk)

		// Get the disk to check delete_on_termination
		d, _ := repo.GetRootDiskByInstanceID(context.Background(), instID)
		if d.DeleteOnTermination {
			// Delete the disk
			_ = repo.DeleteRootDisk(context.Background(), d.DiskID)
		}

		// Verify deleted
		got, _ := repo.GetRootDiskByID(context.Background(), "disk_001")
		if got != nil {
			t.Error("disk should be deleted")
		}
	})

	t.Run("delete_on_termination=false", func(t *testing.T) {
		mem := newMemPool()
		repo := &testRootDiskRepo{mem: mem}

		instID := "inst_to_delete_2"
		disk := &db.RootDiskRow{
			DiskID:              "disk_002",
			InstanceID:          &instID,
			SourceImageID:       "img_test",
			StoragePoolID:       "pool_01",
			StoragePath:         "nfs://filer/vol/disk_002.qcow2",
			SizeGB:              100,
			DeleteOnTermination: false,
			Status:              db.RootDiskStatusAttached,
		}
		_ = repo.CreateRootDisk(context.Background(), disk)

		// Get the disk to check delete_on_termination
		d, _ := repo.GetRootDiskByInstanceID(context.Background(), instID)
		if !d.DeleteOnTermination {
			// Detach the disk instead of delete
			_ = repo.DetachRootDisk(context.Background(), d.DiskID)
		}

		// Verify detached (not deleted)
		got, _ := repo.GetRootDiskByID(context.Background(), "disk_002")
		if got == nil {
			t.Fatal("disk should still exist")
		}
		if got.Status != db.RootDiskStatusDetached {
			t.Errorf("want DETACHED, got %q", got.Status)
		}
		if got.InstanceID != nil {
			t.Error("instance_id should be nil")
		}
	})
}
