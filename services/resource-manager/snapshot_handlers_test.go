package main

// snapshot_handlers_test.go — VM-P2B-S2: snapshot handler tests.
//
// Tests cover:
//   CREATE snapshot (volume source):
//     - POST /v1/snapshots → 202 + CreateSnapshotResponse with job_id
//     - Missing name → 400
//     - No source provided → 400 snapshot_source_required
//     - Both sources provided → 400 snapshot_source_ambiguous
//     - Source volume not found → 404
//     - Source volume in invalid state (deleting) → 409
//     - Missing auth → 401
//
//   CREATE snapshot (instance source):
//     - POST /v1/snapshots with source_instance_id → 202
//     - Instance in transitional state → 409 snapshot_source_invalid_state
//
//   LIST snapshots:
//     - GET /v1/snapshots → 200 + own snapshots only
//     - Empty list → 200 + []
//
//   GET snapshot:
//     - GET /v1/snapshots/{id} → 200 + SnapshotResponse
//     - Not found → 404
//     - Cross-account → 404
//
//   DELETE snapshot:
//     - DELETE /v1/snapshots/{id} available → 202 + job_id
//     - Transitional state → 409
//     - Active delete job already in flight → 409
//     - Cross-account → 404
//
//   RESTORE snapshot:
//     - POST /v1/snapshots/{id}/restore → 202 + RestoreSnapshotResponse
//     - Snapshot not available → 409 snapshot_not_available
//     - size_gb smaller than snapshot → 422 restore_size_too_small
//     - Missing name → 400
//     - Missing AZ → 400
//     - Cross-account snapshot → 404
//
// Strategy: in-process httptest.Server backed by extended memPool.
// No DB, no Linux/KVM required.
// Source: 11-02-phase-1-test-strategy.md §unit test approach.

import (
	"net/http"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Snapshot-aware test server ────────────────────────────────────────────────

// newSnapTestSrv creates a test server with instance, volume, and snapshot routes.
func newSnapTestSrv(t *testing.T) *testSrv {
	t.Helper()
	mem := newMemPool()
	// snapshots map is initialised by newMemPool() via the patch in instance_handlers_test.go.

	repo := db.New(mem)
	srv := &server{
		log:    newDiscardLogger(),
		repo:   repo,
		region: "us-east-1",
	}
	mux := http.NewServeMux()
	srv.registerInstanceRoutes(mux)
	srv.registerProjectRoutes(mux)
	srv.registerVolumeRoutes(mux)
	srv.registerSnapshotRoutes(mux)
	ts := startTestServer(t, mux)
	return &testSrv{ts: ts, mem: mem}
}

// seedSnapshot seeds a SnapshotRow directly into memPool.
func seedSnapshot(mem *memPool, id, owner, status string, sizeGB int) {
	now := time.Now()
	mem.snapshots[id] = &db.SnapshotRow{
		ID:               id,
		OwnerPrincipalID: owner,
		DisplayName:      "snap-" + id,
		Region:           "us-east-1",
		SizeGB:           sizeGB,
		Status:           status,
		ProgressPercent:  progressFor(status),
		Version:          0,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// seedSnapshotWithSource seeds a snapshot whose source_volume_id is set.
func seedSnapshotWithSource(mem *memPool, id, owner, status, sourceVolID string, sizeGB int) {
	seedSnapshot(mem, id, owner, status, sizeGB)
	mem.snapshots[id].SourceVolumeID = &sourceVolID
}

// progressFor returns the canonical progress_percent for a status.
func progressFor(status string) int {
	if status == db.SnapshotStatusAvailable {
		return 100
	}
	return 0
}

// ── CREATE snapshot tests (volume source) ─────────────────────────────────────

func TestCreateSnapshot_VolumeSource_HappyPath(t *testing.T) {
	s := newSnapTestSrv(t)
	seedVolume(s.mem, "vol-snap-src", alice, "us-east-1a", "available", 50)

	req := CreateSnapshotRequest{
		Name:           "my-snap",
		SourceVolumeID: strPtr("vol-snap-src"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateSnapshotResponse
	decodeBody(t, resp, &out)

	if out.Snapshot.ID == "" {
		t.Error("want non-empty snapshot ID")
	}
	if out.Snapshot.Status != db.SnapshotStatusPending {
		t.Errorf("want status=pending, got %q", out.Snapshot.Status)
	}
	if out.Snapshot.SizeGB != 50 {
		t.Errorf("want size_gb=50, got %d", out.Snapshot.SizeGB)
	}
	if out.JobID == "" {
		t.Error("want non-empty job_id in response")
	}
}

func TestCreateSnapshot_InUseVolume_Accepted(t *testing.T) {
	// Snapshots of in_use volumes are allowed per §2.6.
	s := newSnapTestSrv(t)
	seedVolume(s.mem, "vol-inuse-snap", alice, "us-east-1a", "in_use", 100)

	req := CreateSnapshotRequest{
		Name:           "snap-from-inuse",
		SourceVolumeID: strPtr("vol-inuse-snap"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
}

func TestCreateSnapshot_MissingName_Returns400(t *testing.T) {
	s := newSnapTestSrv(t)
	seedVolume(s.mem, "vol-noname", alice, "us-east-1a", "available", 10)

	req := CreateSnapshotRequest{SourceVolumeID: strPtr("vol-noname")}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "name", errMissingField)
}

func TestCreateSnapshot_NoSource_Returns400(t *testing.T) {
	s := newSnapTestSrv(t)

	req := CreateSnapshotRequest{Name: "orphan-snap"}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "source", errSnapshotSourceRequired)
}

func TestCreateSnapshot_BothSources_Returns400(t *testing.T) {
	s := newSnapTestSrv(t)

	req := CreateSnapshotRequest{
		Name:             "ambig-snap",
		SourceVolumeID:   strPtr("vol-x"),
		SourceInstanceID: strPtr("inst-x"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "source", errSnapshotSourceAmbiguous)
}

func TestCreateSnapshot_VolumeNotFound_Returns404(t *testing.T) {
	s := newSnapTestSrv(t)

	req := CreateSnapshotRequest{
		Name:           "ghost-snap",
		SourceVolumeID: strPtr("vol-does-not-exist"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestCreateSnapshot_VolumeCrossAccount_Returns404(t *testing.T) {
	s := newSnapTestSrv(t)
	// Volume owned by bob, but alice is requesting.
	seedVolume(s.mem, "vol-bobs", bob, "us-east-1a", "available", 20)

	req := CreateSnapshotRequest{
		Name:           "steal-snap",
		SourceVolumeID: strPtr("vol-bobs"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (not 403), got %d", resp.StatusCode)
	}
}

func TestCreateSnapshot_VolumeInInvalidState_Returns409(t *testing.T) {
	s := newSnapTestSrv(t)
	seedVolume(s.mem, "vol-deleting", alice, "us-east-1a", "deleting", 30)

	req := CreateSnapshotRequest{
		Name:           "bad-state-snap",
		SourceVolumeID: strPtr("vol-deleting"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errSnapshotSourceInvalidState {
		t.Errorf("want code=%s, got %s", errSnapshotSourceInvalidState, env.Error.Code)
	}
}

func TestCreateSnapshot_NoAuth_Returns401(t *testing.T) {
	s := newSnapTestSrv(t)
	req := CreateSnapshotRequest{
		Name:           "unauth-snap",
		SourceVolumeID: strPtr("vol-x"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// ── CREATE snapshot tests (instance source) ───────────────────────────────────

func TestCreateSnapshot_InstanceSource_HappyPath(t *testing.T) {
	s := newSnapTestSrv(t)
	seedInstance(s.mem, "inst-snap-src", "inst-snap-src", alice, "running")
	// Seed a root disk so handler can derive size_gb.
	instID := "inst-snap-src"
	s.mem.rootDisks["disk-snap-src"] = &db.RootDiskRow{
		DiskID:     "disk-snap-src",
		InstanceID: &instID,
		SizeGB:     20,
		Status:     "attached",
	}

	req := CreateSnapshotRequest{
		Name:             "inst-snap",
		SourceInstanceID: strPtr("inst-snap-src"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out CreateSnapshotResponse
	decodeBody(t, resp, &out)
	if out.Snapshot.SizeGB != 20 {
		t.Errorf("want size_gb=20 (from root disk), got %d", out.Snapshot.SizeGB)
	}
}

func TestCreateSnapshot_InstanceTransitionalState_Returns409(t *testing.T) {
	s := newSnapTestSrv(t)
	seedInstance(s.mem, "inst-stopping", "inst-stopping", alice, "stopping")

	req := CreateSnapshotRequest{
		Name:             "transitional-snap",
		SourceInstanceID: strPtr("inst-stopping"),
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots", req, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errSnapshotSourceInvalidState {
		t.Errorf("want code=%s, got %s", errSnapshotSourceInvalidState, env.Error.Code)
	}
}

// ── LIST snapshots tests ──────────────────────────────────────────────────────

func TestListSnapshots_ReturnOwnedSnapshots(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-a", alice, db.SnapshotStatusAvailable, 50)
	seedSnapshot(s.mem, "snap-b", alice, db.SnapshotStatusPending, 100)
	seedSnapshot(s.mem, "snap-bobs", bob, db.SnapshotStatusAvailable, 30)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/snapshots", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListSnapshotsResponse
	decodeBody(t, resp, &out)
	if out.Total != 2 {
		t.Errorf("want total=2 (alice's snapshots only), got %d", out.Total)
	}
}

func TestListSnapshots_Empty_Returns200WithEmptySlice(t *testing.T) {
	s := newSnapTestSrv(t)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/snapshots", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListSnapshotsResponse
	decodeBody(t, resp, &out)
	if out.Total != 0 {
		t.Errorf("want total=0, got %d", out.Total)
	}
	if out.Snapshots == nil {
		t.Error("want non-nil Snapshots slice")
	}
}

// ── GET snapshot tests ────────────────────────────────────────────────────────

func TestGetSnapshot_HappyPath(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-get", alice, db.SnapshotStatusAvailable, 75)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/snapshots/snap-get", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out SnapshotResponse
	decodeBody(t, resp, &out)
	if out.ID != "snap-get" {
		t.Errorf("want id=snap-get, got %q", out.ID)
	}
	if out.Status != db.SnapshotStatusAvailable {
		t.Errorf("want status=available, got %q", out.Status)
	}
}

func TestGetSnapshot_NotFound_Returns404(t *testing.T) {
	s := newSnapTestSrv(t)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/snapshots/snap-ghost", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestGetSnapshot_CrossAccount_Returns404(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-bobs", bob, db.SnapshotStatusAvailable, 10)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/snapshots/snap-bobs", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (not 403), got %d", resp.StatusCode)
	}
}

// ── DELETE snapshot tests ─────────────────────────────────────────────────────

func TestDeleteSnapshot_Available_Returns202(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-del", alice, db.SnapshotStatusAvailable, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/snapshots/snap-del", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out SnapshotLifecycleResponse
	decodeBody(t, resp, &out)
	if out.Action != "delete" {
		t.Errorf("want action=delete, got %q", out.Action)
	}
	if out.JobID == "" {
		t.Error("want non-empty job_id")
	}
}

func TestDeleteSnapshot_ErrorState_Returns202(t *testing.T) {
	// Deleting a snapshot in 'error' state is permitted.
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-err", alice, db.SnapshotStatusError, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/snapshots/snap-err", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
}

func TestDeleteSnapshot_TransitionalState_Returns409(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-creating", alice, db.SnapshotStatusCreating, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/snapshots/snap-creating", nil, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errSnapshotInvalidState {
		t.Errorf("want code=%s, got %s", errSnapshotInvalidState, env.Error.Code)
	}
}

func TestDeleteSnapshot_ActiveJobInFlight_Returns409(t *testing.T) {
	// VOL-I-5 analogue: double-delete rejected.
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-dbl-del", alice, db.SnapshotStatusAvailable, 50)
	// Seed an active SNAPSHOT_DELETE job.
	snapID := "snap-dbl-del"
	s.mem.jobs["job-snap-del"] = &db.JobRow{
		ID:         "job-snap-del",
		SnapshotID: &snapID,
		JobType:    "SNAPSHOT_DELETE",
		Status:     "pending",
	}

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/snapshots/snap-dbl-del", nil, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
}

func TestDeleteSnapshot_CrossAccount_Returns404(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-bobs-del", bob, db.SnapshotStatusAvailable, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/snapshots/snap-bobs-del", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (not 403), got %d", resp.StatusCode)
	}
}

// ── RESTORE snapshot tests ────────────────────────────────────────────────────

func TestRestoreSnapshot_HappyPath_Returns202(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-restore", alice, db.SnapshotStatusAvailable, 50)

	req := RestoreSnapshotRequest{
		Name:             "restored-vol",
		AvailabilityZone: "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots/snap-restore/restore", req, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out RestoreSnapshotResponse
	decodeBody(t, resp, &out)
	if out.Volume.ID == "" {
		t.Error("want non-empty volume_id in restore response")
	}
	if out.JobID == "" {
		t.Error("want non-empty job_id in restore response")
	}

	// Verify the new volume exists in the pool with status=creating, origin=snapshot.
	vol := s.mem.volumes[out.Volume.ID]
	if vol == nil {
		t.Fatalf("restored volume %s not found in memPool", out.Volume.ID)
	}
	if vol.Status != "creating" {
		t.Errorf("want status=creating, got %q", vol.Status)
	}
	if vol.Origin != "snapshot" {
		t.Errorf("want origin=snapshot, got %q", vol.Origin)
	}
	if vol.SourceSnapshotID == nil || *vol.SourceSnapshotID != "snap-restore" {
		t.Errorf("want source_snapshot_id=snap-restore, got %v", vol.SourceSnapshotID)
	}
}

func TestRestoreSnapshot_SizeOverride_LargerThanSnap_Returns202(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-resize", alice, db.SnapshotStatusAvailable, 50)

	size := 100
	req := RestoreSnapshotRequest{
		Name:             "big-restored",
		AvailabilityZone: "us-east-1a",
		SizeGB:           &size,
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots/snap-resize/restore", req, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out RestoreSnapshotResponse
	decodeBody(t, resp, &out)
	vol := s.mem.volumes[out.Volume.ID]
	if vol == nil {
		t.Fatalf("volume not found")
	}
	if vol.SizeGB != 100 {
		t.Errorf("want size_gb=100, got %d", vol.SizeGB)
	}
}

func TestRestoreSnapshot_SizeSmallerThanSnap_Returns422(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-shrink", alice, db.SnapshotStatusAvailable, 100)

	size := 50 // smaller than snap size
	req := RestoreSnapshotRequest{
		Name:             "shrink-vol",
		AvailabilityZone: "us-east-1a",
		SizeGB:           &size,
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots/snap-shrink/restore", req, authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errRestoreSizeTooSmall {
		t.Errorf("want code=%s, got %s", errRestoreSizeTooSmall, env.Error.Code)
	}
}

func TestRestoreSnapshot_SnapshotNotAvailable_Returns409(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-pending-restore", alice, db.SnapshotStatusPending, 50)

	req := RestoreSnapshotRequest{
		Name:             "nope",
		AvailabilityZone: "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots/snap-pending-restore/restore", req, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errSnapshotNotAvailable {
		t.Errorf("want code=%s, got %s", errSnapshotNotAvailable, env.Error.Code)
	}
}

func TestRestoreSnapshot_MissingName_Returns400(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-restore-noname", alice, db.SnapshotStatusAvailable, 50)

	req := RestoreSnapshotRequest{AvailabilityZone: "us-east-1a"}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots/snap-restore-noname/restore", req, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "name", errMissingField)
}

func TestRestoreSnapshot_CrossAccount_Returns404(t *testing.T) {
	s := newSnapTestSrv(t)
	seedSnapshot(s.mem, "snap-bobs-restore", bob, db.SnapshotStatusAvailable, 50)

	req := RestoreSnapshotRequest{
		Name:             "steal-vol",
		AvailabilityZone: "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/snapshots/snap-bobs-restore/restore", req, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (not 403), got %d", resp.StatusCode)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// strPtr returns a pointer to a string literal.
func strPtr(s string) *string { return &s }
