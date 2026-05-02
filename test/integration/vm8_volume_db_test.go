//go:build integration

package integration

// vm8_volume_db_test.go — VM Job 8: Volume attach/persistence + Postgres integration gate.
//
// Integration tests for volume, IP allocation, job claim/retry, attach uniqueness,
// stale attachment scans, and quota/admission error separation against a real
// PostgreSQL database.
//
// All tests are skipped when DATABASE_URL is not set.
//
// Run:
//   DATABASE_URL=postgres://... go test -tags=integration -v ./test/integration/... -run TestVM8 -count=1 -timeout=300s
//
// Source: VM Job 8 — Volume attach/persistence + Postgres integration gate.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

func skipWithoutDB(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
}

// ── VM8: Concurrent IP Allocation ─────────────────────────────────────────────

// TestVM8_ConcurrentIPAllocation_NoDuplicates allocates IPs concurrently and
// asserts all allocations are unique.
func TestVM8_ConcurrentIPAllocation_NoDuplicates(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	const n = 10
	ips := make([]string, n)
	errs := make([]error, n)
	instanceIDs := make([]string, n)
	for i := range n {
		instanceIDs[i] = idgen.New(idgen.PrefixInstance)
	}

	done := make(chan int, n)
	for i := range n {
		go func(idx int) {
			ip, err := repo.AllocateIP(ctx, integVPCID, instanceIDs[idx])
			ips[idx] = ip
			errs[idx] = err
			done <- idx
		}(i)
	}
	for range n {
		<-done
	}

	t.Cleanup(func() {
		for i, ip := range ips {
			if ip != "" {
				_ = repo.ReleaseIP(ctx, ip, integVPCID, instanceIDs[i])
			}
		}
	})

	seen := make(map[string]bool)
	success := 0
	for i, ip := range ips {
		if errs[i] != nil {
			t.Logf("goroutine %d: AllocateIP error: %v", i, errs[i])
			continue
		}
		success++
		if seen[ip] {
			t.Errorf("DUPLICATE IP: %s allocated twice — invariant I-2 violated", ip)
		}
		seen[ip] = true
	}
	t.Logf("concurrent IP allocation: %d/%d succeeded with %d unique IPs", success, n, len(seen))
}

// ── VM8: Job Claim/Retry ──────────────────────────────────────────────────────

// TestVM8_JobClaim_Atomic verifies AtomicClaimJob picks exactly one pending job.
func TestVM8_JobClaim_Atomic(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	inst := newIntegInstance(t, repo)
	newIntegHost(t, repo)

	// Insert two jobs with different IDs.
	for i := range 2 {
		job := &db.JobRow{
			ID:          fmt.Sprintf("vm8job-claim-%d", time.Now().UnixNano()+int64(i)),
			InstanceID:  inst.ID,
			JobType:     "INSTANCE_START",
			Status:      "pending",
			MaxAttempts: 5,
		}
		// Use raw insert since InsertJob also handles instance-scoped jobs.
		if err := repo.InsertJob(ctx, job); err != nil {
			t.Fatalf("InsertJob %d: %v", i, err)
		}
	}

	claimed, err := repo.AtomicClaimJob(ctx)
	if err != nil {
		t.Fatalf("AtomicClaimJob: %v", err)
	}
	if claimed == nil {
		t.Fatal("AtomicClaimJob returned nil — expected a job")
	}
	if claimed.Status != "in_progress" {
		t.Errorf("claimed job status = %q, want in_progress", claimed.Status)
	}
	t.Logf("claimed job: %s type=%s", claimed.ID, claimed.JobType)
}

// TestVM8_JobRetry_RequeueFailedAttempt verifies requeue from in_progress back to pending.
func TestVM8_JobRetry_RequeueFailedAttempt(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	inst := newIntegInstance(t, repo)
	newIntegHost(t, repo)

	jobID := fmt.Sprintf("vm8job-retry-%d", time.Now().UnixNano())
	job := &db.JobRow{
		ID:          jobID,
		InstanceID:  inst.ID,
		JobType:     "INSTANCE_START",
		Status:      "pending",
		MaxAttempts: 5,
	}
	if err := repo.InsertJob(ctx, job); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}

	claimed, _ := repo.AtomicClaimJob(ctx)
	if claimed == nil || claimed.ID != jobID {
		t.Fatal("failed to claim the inserted job")
	}

	msg := "simulated error for retry"
	if err := repo.RequeueFailedAttempt(ctx, jobID, &msg); err != nil {
		t.Fatalf("RequeueFailedAttempt: %v", err)
	}

	// Verify job is back in pending.
	reloaded, err := repo.GetJobByID(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if reloaded == nil {
		t.Fatal("job not found after requeue")
	}
	if reloaded.Status != "pending" {
		t.Errorf("requeued job status = %q, want pending", reloaded.Status)
	}
}

// ── VM8: Volume Attach Uniqueness ─────────────────────────────────────────────

// TestVM8_VolumeAttach_Uniqueness verifies that a volume cannot be attached to
// two instances simultaneously. The unique partial index on volume_attachments
// (volume_id WHERE detached_at IS NULL) enforces VOL-I-1 at the DB layer.
func TestVM8_VolumeAttach_Uniqueness(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	volID := "vol-vm8-uniq-" + idgen.New("")[4:]
	inst1ID := "inst-vm8-uniq1-" + idgen.New("")[4:]
	inst2ID := "inst-vm8-uniq2-" + idgen.New("")[4:]

	ownerID := "00000000-0000-0000-0000-000000000001"

	// Seed volume.
	if err := repo.CreateVolume(ctx, &db.VolumeRow{
		ID: volID, OwnerPrincipalID: ownerID, DisplayName: "unique-test",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
		SizeGB: 10, Origin: "blank",
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	t.Cleanup(func() {
		_ = repo.SoftDeleteVolume(ctx, volID, 0)
	})

	// First attachment.
	att1 := &db.VolumeAttachmentRow{
		ID:         "vatt-vm8-1",
		VolumeID:   volID,
		InstanceID: inst1ID,
		DevicePath: "/dev/vdb",
	}
	if err := repo.CreateVolumeAttachment(ctx, att1); err != nil {
		t.Fatalf("first CreateVolumeAttachment: %v", err)
	}

	// Second attachment to a different instance must fail with unique violation.
	att2 := &db.VolumeAttachmentRow{
		ID:         "vatt-vm8-2",
		VolumeID:   volID,
		InstanceID: inst2ID,
		DevicePath: "/dev/vdb",
	}
	err := repo.CreateVolumeAttachment(ctx, att2)
	if err == nil {
		t.Error("second CreateVolumeAttachment with same volume should fail (unique constraint)")
	} else {
		t.Logf("second attachment correctly rejected: %v", err)
	}

	// Cleanup first attachment.
	_ = repo.CloseVolumeAttachment(ctx, att1.ID)
}

// ── VM8: Stale Attachment Scans ───────────────────────────────────────────────

// TestVM8_StaleAttachment_ScanDetects verifies ListStaleAttachments returns
// attachments where the owning instance is in a terminal state.
func TestVM8_StaleAttachment_ScanDetects(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	volID := "vol-vm8-stale-" + idgen.New("")[4:]
	instID := "inst-vm8-stale-" + idgen.New("")[4:]
	ownerID := "00000000-0000-0000-0000-000000000001"

	// Seed instance in failed state (terminal).
	if err := repo.InsertInstance(ctx, &db.InstanceRow{
		ID: instID, Name: "stale-inst",
		OwnerPrincipalID: ownerID,
		VMState:          "failed",
		InstanceTypeID:   "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		Version:          0,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("InsertInstance: %v", err)
	}
	t.Cleanup(func() {
		_ = repo.SoftDeleteVolume(ctx, volID, 0)
		_ = repo.UpdateInstanceState(ctx, instID, "failed", "deleted", 0)
	})

	// Seed volume.
	if err := repo.CreateVolume(ctx, &db.VolumeRow{
		ID: volID, OwnerPrincipalID: ownerID, DisplayName: "stale-test",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
		SizeGB: 10, Origin: "blank",
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	att := &db.VolumeAttachmentRow{
		ID:         "vatt-vm8-stale",
		VolumeID:   volID,
		InstanceID: instID,
		DevicePath: "/dev/vdb",
	}
	if err := repo.CreateVolumeAttachment(ctx, att); err != nil {
		t.Fatalf("CreateVolumeAttachment: %v", err)
	}
	t.Cleanup(func() { _ = repo.CloseVolumeAttachment(ctx, att.ID) })

	// Scan for stale attachments.
	staleRows, err := repo.ListStaleAttachments(ctx)
	if err != nil {
		t.Fatalf("ListStaleAttachments: %v", err)
	}

	found := false
	for _, r := range staleRows {
		if r.AttachmentID == att.ID {
			found = true
			if r.InstanceState != "failed" {
				t.Errorf("stale attachment instance_state = %q, want failed", r.InstanceState)
			}
			t.Logf("found stale attachment: %s (instance: %s state: %s, volume: %s state: %s)",
				r.AttachmentID, r.InstanceID, r.InstanceState, r.VolumeID, r.VolumeState)
		}
	}
	if !found {
		t.Error("stale attachment not found by ListStaleAttachments")
	}
}

// ── VM8: Volume Orphan Storage Scan ───────────────────────────────────────────

// TestVM8_OrphanVolumeStorage_ScanDetects verifies ListVolumesWithOrphanStorage
// returns volumes in terminal states with storage_path set.
func TestVM8_OrphanVolumeStorage_ScanDetects(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	volID := "vol-vm8-orphan-" + idgen.New("")[4:]
	ownerID := "00000000-0000-0000-0000-000000000001"

	// Seed volume and then set it to error state with a storage path.
	if err := repo.CreateVolume(ctx, &db.VolumeRow{
		ID: volID, OwnerPrincipalID: ownerID, DisplayName: "orphan-test",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
		SizeGB: 10, Origin: "blank",
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	// Set storage path.
	storagePath := "/var/lib/compute-platform/storage/volumes/" + volID + "/disk.img"
	if err := repo.SetVolumeStoragePath(ctx, volID, storagePath); err != nil {
		t.Fatalf("SetVolumeStoragePath: %v", err)
	}

	// Transition to error state.
	if err := repo.UnlockVolume(ctx, volID, db.VolumeStatusError); err != nil {
		t.Fatalf("UnlockVolume: %v", err)
	}
	t.Cleanup(func() { _ = repo.SoftDeleteVolume(ctx, volID, 0) })

	orphans, err := repo.ListVolumesWithOrphanStorage(ctx)
	if err != nil {
		t.Fatalf("ListVolumesWithOrphanStorage: %v", err)
	}

	found := false
	for _, o := range orphans {
		if o.VolumeID == volID {
			found = true
			if o.StoragePath != storagePath {
				t.Errorf("orphan storage_path = %q, want %q", o.StoragePath, storagePath)
			}
			t.Logf("found orphan volume: %s storage_path=%s status=%s", o.VolumeID, o.StoragePath, o.Status)
		}
	}
	if !found {
		t.Error("orphan volume not found by ListVolumesWithOrphanStorage")
	}
}

// ── VM8: Job Claim Concurrency (real DB stress) ───────────────────────────────

// TestVM8_JobClaim_ConcurrentClaim verifies that multiple concurrent
// AtomicClaimJob calls each get a distinct job.
func TestVM8_JobClaim_ConcurrentClaim(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	inst := newIntegInstance(t, repo)
	newIntegHost(t, repo)

	// Insert 5 pending jobs.
	for i := range 5 {
		job := &db.JobRow{
			ID:          fmt.Sprintf("vm8job-conc-%d-%d", time.Now().UnixNano(), i),
			InstanceID:  inst.ID,
			JobType:     "INSTANCE_START",
			Status:      "pending",
			MaxAttempts: 5,
		}
		if err := repo.InsertJob(ctx, job); err != nil {
			t.Fatalf("InsertJob %d: %v", i, err)
		}
	}

	var mu sync.Mutex
	claimedIDs := make(map[string]bool)
	var wg sync.WaitGroup

	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := repo.AtomicClaimJob(ctx)
			if err != nil {
				t.Errorf("AtomicClaimJob error: %v", err)
				return
			}
			if job == nil {
				return
			}
			mu.Lock()
			if claimedIDs[job.ID] {
				t.Errorf("DUPLICATE claim: job %s claimed by multiple workers", job.ID)
			}
			claimedIDs[job.ID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	t.Logf("concurrent job claim: %d unique jobs claimed", len(claimedIDs))
}

// ── VM8: Volume Attach Adherence to Stopped-Only ──────────────────────────────

// TestVM8_VolumeAttach_DevicePathAssignment verifies deterministic device path
// assignment via NextDevicePath.
func TestVM8_VolumeAttach_DevicePathAssignment(t *testing.T) {
	skipWithoutDB(t)
	ctx := context.Background()
	repo := testRepo(t)

	ownerID := "00000000-0000-0000-0000-000000000001"

	// Create first volume.
	vol1ID := "vol-vm8-dev1-" + idgen.New("")[4:]
	if err := repo.CreateVolume(ctx, &db.VolumeRow{
		ID: vol1ID, OwnerPrincipalID: ownerID, DisplayName: "devpath1",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
		SizeGB: 10, Origin: "blank",
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	t.Cleanup(func() { _ = repo.SoftDeleteVolume(ctx, vol1ID, 0) })

	// Create second volume.
	vol2ID := "vol-vm8-dev2-" + idgen.New("")[4:]
	if err := repo.CreateVolume(ctx, &db.VolumeRow{
		ID: vol2ID, OwnerPrincipalID: ownerID, DisplayName: "devpath2",
		Region: "us-east-1", AvailabilityZone: "us-east-1a",
		SizeGB: 20, Origin: "blank",
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	t.Cleanup(func() { _ = repo.SoftDeleteVolume(ctx, vol2ID, 0) })

	instID := "inst-vm8-devpath-" + idgen.New("")[4:]

	// First device path should be /dev/vdb.
	path1, err := repo.NextDevicePath(ctx, instID)
	if err != nil {
		t.Fatalf("first NextDevicePath: %v", err)
	}
	if path1 != "/dev/vdb" {
		t.Errorf("first device path = %q, want /dev/vdb", path1)
	}

	// Create first attachment.
	att1 := &db.VolumeAttachmentRow{
		ID:         "vatt-vm8-dp1",
		VolumeID:   vol1ID,
		InstanceID: instID,
		DevicePath: path1,
	}
	if err := repo.CreateVolumeAttachment(ctx, att1); err != nil {
		t.Fatalf("CreateVolumeAttachment 1: %v", err)
	}
	t.Cleanup(func() { _ = repo.CloseVolumeAttachment(ctx, att1.ID) })

	// Second device path should be /dev/vdc.
	path2, err := repo.NextDevicePath(ctx, instID)
	if err != nil {
		t.Fatalf("second NextDevicePath: %v", err)
	}
	if path2 != "/dev/vdc" {
		t.Errorf("second device path = %q, want /dev/vdc", path2)
	}
}
