package main

// volume_handlers_test.go — VM-VOLUME-RUNTIME-F: volume API handler tests.
//
// Tests cover:
//   CREATE volume:
//     - POST /v1/volumes → 202 + CreateVolumeResponse + job_id
//     - Missing name → 400
//     - Invalid size_gb → 400
//     - Missing AZ → 400
//     - Missing auth → 401
//
//   LIST volumes:
//     - GET /v1/volumes → 200 + own volumes only
//     - Empty list → 200 + []
//
//   GET volume:
//     - GET /v1/volumes/{id} → 200 + VolumeResponse
//     - Not found → 404
//     - Cross-account → 404
//
//   DELETE volume:
//     - DELETE /v1/volumes/{id} available → 202 + job_id
//     - Volume in_use → 409 volume_in_use
//     - Volume transitional → 409
//     - Cross-account → 404
//
//   ATTACH volume:
//     - POST /v1/instances/{id}/volumes stopped instance → 202 + job_id
//     - Cross-AZ attach → 422 volume_az_mismatch
//     - Already attached volume → 409 volume_already_attached
//     - Hot attach (non-stopped instance) → 409 illegal_state_transition
//     - Volume not available → 409
//     - Volume limit exceeded → 422
//
//   DETACH volume:
//     - DELETE /v1/instances/{id}/volumes/{vol_id} stopped instance → 202 + job_id
//     - Hot detach (non-stopped instance) → 409
//     - Volume not attached → 409
//     - Cross-account volume → 404
//
//   LIST instance volumes:
//     - GET /v1/instances/{id}/volumes → 200 + attachment entries
//
// Strategy: in-process httptest.Server backed by memPool (fake db.Pool).
// No DB, no Linux/KVM required.
// Source: 11-02-phase-1-test-strategy.md §unit test approach,
//         P2_VOLUME_MODEL.md §8 (API endpoint summary).

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Volume-aware test server ───────────────────────────────────────────────────

// newVolTestSrv creates a test server with instance, volume, and snapshot routes.
func newVolTestSrv(t *testing.T) *testSrv {
	t.Helper()
	mem := newMemPool()
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

// ── Seed helpers ───────────────────────────────────────────────────────────────

// seedInstance creates a stopped instance row in memPool for volume attach/detach tests.
func seedStoppedInstance(mem *memPool, id, owner string) {
	now := time.Now()
	mem.instances[id] = &db.InstanceRow{
		ID:               id,
		Name:             "inst-" + id,
		OwnerPrincipalID: owner,
		VMState:          "stopped",
		InstanceTypeID:   "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// seedRunningInstance creates a running instance row for hot-attach rejection tests.
func seedRunningInstance(mem *memPool, id, owner string) {
	now := time.Now()
	mem.instances[id] = &db.InstanceRow{
		ID:               id,
		Name:             "inst-" + id,
		OwnerPrincipalID: owner,
		VMState:          "running",
		InstanceTypeID:   "c1.small",
		ImageID:          "00000000-0000-0000-0000-000000000010",
		AvailabilityZone: "us-east-1a",
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// seedVolume creates a volume row in memPool.
func seedVol(mem *memPool, id, owner, az, status string, sizeGB int) {
	now := time.Now()
	mem.volumes[id] = &db.VolumeRow{
		ID:               id,
		OwnerPrincipalID: owner,
		DisplayName:      "vol-" + id,
		Region:           "us-east-1",
		AvailabilityZone: az,
		SizeGB:           sizeGB,
		Status:           status,
		Origin:           "blank",
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// seedAttachment creates an active attachment row in memPool.
func seedAttachment(mem *memPool, attID, volID, instID, devicePath string) {
	now := time.Now()
	mem.volumeAttachments[attID] = &db.VolumeAttachmentRow{
		ID:                  attID,
		VolumeID:            volID,
		InstanceID:          instID,
		DevicePath:          devicePath,
		DeleteOnTermination: false,
		AttachedAt:          now,
	}
}

// decodeJSONTo reads the response body into a map.
func decodeJSONTo(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decodeJSONTo: %v", err)
	}
	return out
}

// assertStatus fails the test if resp.StatusCode != want.
func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Errorf("status = %d, want %d", resp.StatusCode, want)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// CREATE volume tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestCreateVolume_HappyPath_Returns202(t *testing.T) {
	s := newVolTestSrv(t)
	body := map[string]any{
		"name":              "prod-data",
		"size_gb":           100,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusAccepted)

	// Verify response shape.
	out := decodeJSONTo(t, resp)
	if out["volume"] == nil {
		t.Fatal("missing volume field in response")
	}
	if out["job_id"] == nil {
		t.Fatal("missing job_id field in response")
	}
}

func TestCreateVolume_MissingName_Returns400(t *testing.T) {
	s := newVolTestSrv(t)
	body := map[string]any{
		"size_gb":           100,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestCreateVolume_InvalidSize_Returns400(t *testing.T) {
	s := newVolTestSrv(t)
	body := map[string]any{
		"name":              "bad-size",
		"size_gb":           0,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestCreateVolume_MissingAZ_Returns400(t *testing.T) {
	s := newVolTestSrv(t)
	body := map[string]any{
		"name":    "no-az",
		"size_gb": 50,
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestCreateVolume_NoAuth_Returns401(t *testing.T) {
	s := newVolTestSrv(t)
	body := map[string]any{
		"name":              "no-auth",
		"size_gb":           100,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, nil)
	assertStatus(t, resp, http.StatusUnauthorized)
}

// ═══════════════════════════════════════════════════════════════════════════════
// LIST volumes tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestListVolumes_ReturnsOwnVolumes(t *testing.T) {
	s := newVolTestSrv(t)
	seedVol(s.mem, "vol-alice-1", alice, "us-east-1a", db.VolumeStatusAvailable, 50)
	seedVol(s.mem, "vol-bob-1", bob, "us-east-1a", db.VolumeStatusAvailable, 100)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/volumes", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusOK)

	out := decodeJSONTo(t, resp)
	volumes, ok := out["volumes"].([]any)
	if !ok {
		t.Fatal("missing volumes array")
	}
	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume for alice, got %d", len(volumes))
	}
	if total := int(out["total"].(float64)); total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
}

func TestListVolumes_Empty_Returns200(t *testing.T) {
	s := newVolTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/volumes", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusOK)

	out := decodeJSONTo(t, resp)
	volumes, ok := out["volumes"].([]any)
	if !ok {
		t.Fatal("missing volumes array")
	}
	if len(volumes) != 0 {
		t.Fatalf("expected 0 volumes, got %d", len(volumes))
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// GET volume tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestGetVolume_HappyPath_Returns200(t *testing.T) {
	s := newVolTestSrv(t)
	seedVol(s.mem, "vol-get-happy", alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/volumes/vol-get-happy", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusOK)
}

func TestGetVolume_NotFound_Returns404(t *testing.T) {
	s := newVolTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/volumes/nonexistent", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusNotFound)
}

func TestGetVolume_CrossAccount_Returns404(t *testing.T) {
	s := newVolTestSrv(t)
	seedVol(s.mem, "vol-bob-private", bob, "us-east-1a", db.VolumeStatusAvailable, 100)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/volumes/vol-bob-private", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusNotFound)
}

// ═══════════════════════════════════════════════════════════════════════════════
// DELETE volume tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestDeleteVolume_Available_Returns202(t *testing.T) {
	s := newVolTestSrv(t)
	seedVol(s.mem, "vol-del-ok", alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/volumes/vol-del-ok", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusAccepted)
}

func TestDeleteVolume_InUse_Returns409(t *testing.T) {
	s := newVolTestSrv(t)
	seedVol(s.mem, "vol-inuse", alice, "us-east-1a", db.VolumeStatusInUse, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/volumes/vol-inuse", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusConflict)
}

func TestDeleteVolume_Transitional_Returns409(t *testing.T) {
	s := newVolTestSrv(t)
	seedVol(s.mem, "vol-attaching", alice, "us-east-1a", db.VolumeStatusAttaching, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/volumes/vol-attaching", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusConflict)
}

func TestDeleteVolume_CrossAccount_Returns404(t *testing.T) {
	s := newVolTestSrv(t)
	seedVol(s.mem, "vol-bobs", bob, "us-east-1a", db.VolumeStatusAvailable, 100)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/volumes/vol-bobs", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusNotFound)
}

// ═══════════════════════════════════════════════════════════════════════════════
// ATTACH volume tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestAttachVolume_StoppedInstance_Returns202(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-attach-ok"
	const volID = "vol-attach-ok"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	body := map[string]any{"volume_id": volID}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/"+instID+"/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusAccepted)
}

func TestAttachVolume_CrossAZ_Rejected(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-cross-az"
	const volID = "vol-cross-az"
	seedStoppedInstance(s.mem, instID, alice)
	// Volume in us-east-1b while instance is in us-east-1a.
	seedVol(s.mem, volID, alice, "us-east-1b", db.VolumeStatusAvailable, 50)

	body := map[string]any{"volume_id": volID}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/"+instID+"/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusUnprocessableEntity)
}

func TestAttachVolume_AlreadyAttached_Rejected(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-already-att"
	const volID = "vol-already-att"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusInUse, 50)

	body := map[string]any{"volume_id": volID}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/"+instID+"/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusConflict)
}

func TestAttachVolume_HotAttach_Rejected(t *testing.T) {
	// Running-instance hot attach is explicitly unsupported.
	// The API must return deterministic conflict/unsupported behavior.
	s := newVolTestSrv(t)
	const instID = "inst-hot-att"
	const volID = "vol-hot-att"
	seedRunningInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	body := map[string]any{"volume_id": volID}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/"+instID+"/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusConflict)
}

func TestAttachVolume_NotAvailable_Rejected(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-not-avail"
	const volID = "vol-creating"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusCreating, 50)

	body := map[string]any{"volume_id": volID}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/"+instID+"/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusConflict)
}

func TestAttachVolume_VolumeNotFound_Returns404(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-vol-404"
	seedStoppedInstance(s.mem, instID, alice)

	body := map[string]any{"volume_id": "vol-nonexistent"}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/"+instID+"/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusNotFound)
}

func TestAttachVolume_CrossAccount_Returns404(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-cross-acct"
	const volID = "vol-bobs-vol"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, bob, "us-east-1a", db.VolumeStatusAvailable, 50)

	body := map[string]any{"volume_id": volID}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/"+instID+"/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusNotFound)
}

// ═══════════════════════════════════════════════════════════════════════════════
// DETACH volume tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestDetachVolume_StoppedInstance_Returns202(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-detach-ok"
	const volID = "vol-detach-ok"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusInUse, 50)
	seedAttachment(s.mem, "vatt-detach-ok", volID, instID, "/dev/vdb")

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/"+instID+"/volumes/"+volID, nil, authHdr(alice))
	assertStatus(t, resp, http.StatusAccepted)
}

func TestDetachVolume_HotDetach_Rejected(t *testing.T) {
	// Running-instance hot detach is explicitly unsupported.
	s := newVolTestSrv(t)
	const instID = "inst-hot-det"
	const volID = "vol-hot-det"
	seedRunningInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusInUse, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/"+instID+"/volumes/"+volID, nil, authHdr(alice))
	assertStatus(t, resp, http.StatusConflict)
}

func TestDetachVolume_NotAttached_Returns409(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-not-att"
	const volID = "vol-not-att"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/"+instID+"/volumes/"+volID, nil, authHdr(alice))
	assertStatus(t, resp, http.StatusConflict)
}

func TestDetachVolume_CrossAccount_Returns404(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-det-cross"
	const volID = "vol-det-cross"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, bob, "us-east-1a", db.VolumeStatusInUse, 50)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/"+instID+"/volumes/"+volID, nil, authHdr(alice))
	assertStatus(t, resp, http.StatusNotFound)
}

// ═══════════════════════════════════════════════════════════════════════════════
// LIST instance volumes tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestListInstanceVolumes_HappyPath_Returns200(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-list-vols"
	const volID = "vol-list-vols"
	seedStoppedInstance(s.mem, instID, alice)
	seedVol(s.mem, volID, alice, "us-east-1a", db.VolumeStatusInUse, 50)
	seedAttachment(s.mem, "vatt-list", volID, instID, "/dev/vdb")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/"+instID+"/volumes", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusOK)

	out := decodeJSONTo(t, resp)
	volumes, ok := out["volumes"].([]any)
	if !ok {
		t.Fatal("missing volumes array")
	}
	if len(volumes) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(volumes))
	}
	if total := int(out["total"].(float64)); total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
}

func TestListInstanceVolumes_Empty_Returns200(t *testing.T) {
	s := newVolTestSrv(t)
	const instID = "inst-empty-vols"
	seedStoppedInstance(s.mem, instID, alice)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/"+instID+"/volumes", nil, authHdr(alice))
	assertStatus(t, resp, http.StatusOK)

	out := decodeJSONTo(t, resp)
	volumes, ok := out["volumes"].([]any)
	if !ok {
		t.Fatal("missing volumes array")
	}
	if len(volumes) != 0 {
		t.Fatalf("expected 0 volumes, got %d", len(volumes))
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// CREATE volume — quota tests
// ═══════════════════════════════════════════════════════════════════════════════

// seedVolQuota sets a per-scope max_volume_gb override via the in-memory quota map.
// Must also seed a quota row (via memPool.quotas) for GetQuota to return a row.
func seedVolQuota(mem *memPool, scopeID string, maxVolumeGB int) {
	mem.quotas[scopeID] = db.DefaultMaxInstances
	mem.volumeQuotaMaxGB[scopeID] = maxVolumeGB
}

// TestCreateVolume_UnderQuota_Succeeds verifies a volume create succeeds
// when the scope has available volume GB quota capacity.
func TestCreateVolume_UnderQuota_Succeeds(t *testing.T) {
	s := newVolTestSrv(t)
	// Set volume GB quota to 200 for alice; her existing volume is 50 GB.
	seedVolQuota(s.mem, alice, 200)
	seedVol(s.mem, "vol-quota-ok", alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	body := map[string]any{
		"name":              "under-quota",
		"size_gb":           100,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusAccepted)
}

// TestCreateVolume_QuotaExceeded_Returns422 verifies quota admission for volume
// create returns 422 quota_exceeded when the scope would exceed its volume GB limit.
func TestCreateVolume_QuotaExceeded_Returns422(t *testing.T) {
	s := newVolTestSrv(t)
	// Set tight volume GB quota: 100 GB, already 50 GB consumed.
	seedVolQuota(s.mem, alice, 100)
	seedVol(s.mem, "vol-quota-heavy", alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	// Request 100 GB more → would be 150, exceeding 100 limit.
	body := map[string]any{
		"name":              "over-quota",
		"size_gb":           100,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusUnprocessableEntity)

	// Verify the error code is quota_exceeded.
	var env apiError
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("code = %q, want %q", env.Error.Code, errQuotaExceeded)
	}
}

// TestCreateVolume_QuotaCheckBeforeInsert verifies that quota admission
// happens before the volume row is inserted — when quota is exceeded,
// no volume row is written to the store.
func TestCreateVolume_QuotaCheckBeforeInsert(t *testing.T) {
	s := newVolTestSrv(t)
	// Exhaust quota: only 50 GB allowed, already consumed.
	seedVolQuota(s.mem, alice, 50)
	seedVol(s.mem, "vol-full", alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	body := map[string]any{
		"name":              "should-not-exist",
		"size_gb":           10,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusUnprocessableEntity)

	// Verify no new volume was created beyond the original two entries
	// (the seeded one + the one from seedVolQuota which only touches quotas).
	count := 0
	for _, vol := range s.mem.volumes {
		if vol.OwnerPrincipalID == alice && vol.DeletedAt == nil &&
			vol.Status != "deleted" && vol.Status != "failed" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("volume count = %d, want 1 (no new volume inserted when quota exceeded)", count)
	}
}

// TestCreateVolume_QuotaIsolated_CrossAccount verifies that volume quota
// exhaustion for one principal does not block volume creation for another.
func TestCreateVolume_QuotaIsolated_CrossAccount(t *testing.T) {
	s := newVolTestSrv(t)
	// Exhaust bob's volume quota.
	seedVolQuota(s.mem, bob, 100)
	seedVol(s.mem, "vol-bob-greedy", bob, "us-east-1a", db.VolumeStatusAvailable, 100)

	// Alice should be unaffected.
	body := map[string]any{
		"name":              "alice-still-ok",
		"size_gb":           100,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusAccepted)
}

// TestCreateVolume_QuotaExceeded_DistinctFromCapacityFailure verifies that
// the quota_exceeded error code is distinct from any capacity-related error code.
func TestCreateVolume_QuotaExceeded_DistinctFromCapacityFailure(t *testing.T) {
	s := newVolTestSrv(t)
	seedVolQuota(s.mem, alice, 50)
	seedVol(s.mem, "vol-cap", alice, "us-east-1a", db.VolumeStatusAvailable, 50)

	body := map[string]any{
		"name":              "distinct-code",
		"size_gb":           10,
		"availability_zone": "us-east-1a",
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/volumes", body, authHdr(alice))
	assertStatus(t, resp, http.StatusUnprocessableEntity)

	var env apiError
	json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("quota failure must not be coded as capacity failure, got %q", env.Error.Code)
	}
	if env.Error.Code == "insufficient_capacity" || env.Error.Code == errServiceUnavailable {
		t.Errorf("code = %q must not be capacity/unavailable", env.Error.Code)
	}
}
