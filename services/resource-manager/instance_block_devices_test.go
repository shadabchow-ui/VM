package main

// instance_block_devices_test.go — M10 Slice 4: block_devices API request path tests.
//
// Tests verify:
//   - Omitted block_devices: Phase 1 default synthesized (backward compatibility)
//   - Explicit block_devices with delete_on_termination=true: accepted
//   - Explicit block_devices with delete_on_termination=false: rejected (Phase 1)
//   - Multiple block device entries: rejected (Phase 1: exactly one)
//   - size_gb validation: > 0 required, <= shape max enforced
//   - image_id mismatch between top-level and block_devices: rejected
//   - block_devices round-trip in create response
//   - block_devices enrichment in GET /v1/instances/{id}
//   - block_devices enrichment in GET /v1/instances (list)
//   - block_devices enrichment with root_disk record present
//   - block_devices enrichment for pre-Slice-4 instances (no root_disk record)
//
// Source: INSTANCE_MODEL_V1 §2, execution_blueprint §7.7,
//         API_ERROR_CONTRACT_V1 §4, P2_VOLUME_MODEL §1,
//         P2_MIGRATION_COMPATIBILITY_RULES §7.2,
//         11-02-phase-1-test-strategy.md §unit test approach.

import (
	"net/http"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── M10 Slice 4: Block device backward compatibility ────────────────────────

func TestCreate_BlockDevices_Omitted_DefaultSynthesized(t *testing.T) {
	// When block_devices is omitted, the handler synthesizes a default
	// from image_id + shape disk size + delete_on_termination=true.
	// This preserves backward compatibility with existing clients.
	s := newTestSrv(t)
	body := validCreateBody() // no BlockDevices field

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	// Verify block_devices is present with synthesized defaults.
	if len(out.Instance.BlockDevices) != 1 {
		t.Fatalf("want 1 block device (synthesized), got %d", len(out.Instance.BlockDevices))
	}

	bd := out.Instance.BlockDevices[0]
	if bd.ImageID != "00000000-0000-0000-0000-000000000010" {
		t.Errorf("want image_id matching top-level, got %q", bd.ImageID)
	}
	if bd.SizeGB != 50 { // c1.small → 50 GB
		t.Errorf("want size_gb=50 (c1.small default), got %d", bd.SizeGB)
	}
	if !bd.DeleteOnTermination {
		t.Error("want delete_on_termination=true (Phase 1 default)")
	}
}

func TestCreate_BlockDevices_Explicit_DeleteOnTerminationTrue(t *testing.T) {
	// Explicit block_devices with delete_on_termination=true: accepted.
	s := newTestSrv(t)
	body := validCreateBody()
	body.BlockDevices = []BlockDeviceMapping{
		{
			ImageID:             "00000000-0000-0000-0000-000000000010",
			SizeGB:              50,
			DeleteOnTermination: true,
		},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	if len(out.Instance.BlockDevices) != 1 {
		t.Fatalf("want 1 block device, got %d", len(out.Instance.BlockDevices))
	}
	if !out.Instance.BlockDevices[0].DeleteOnTermination {
		t.Error("want delete_on_termination=true")
	}
}

func TestCreate_BlockDevices_DeleteOnTerminationFalse_Rejected(t *testing.T) {
	// Phase 1: delete_on_termination=false is rejected with 400.
	// Source: P2_VOLUME_MODEL §1, API_ERROR_CONTRACT_V1 §4 (delete_on_termination_required).
	s := newTestSrv(t)
	body := validCreateBody()
	body.BlockDevices = []BlockDeviceMapping{
		{
			ImageID:             "00000000-0000-0000-0000-000000000010",
			SizeGB:              50,
			DeleteOnTermination: false,
		},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "block_devices[0].delete_on_termination", errDeleteOnTerminationRequired)
}

func TestCreate_BlockDevices_MultipleEntries_Rejected(t *testing.T) {
	// Phase 1: exactly one block device entry. Multiple entries rejected.
	s := newTestSrv(t)
	body := validCreateBody()
	body.BlockDevices = []BlockDeviceMapping{
		{ImageID: "00000000-0000-0000-0000-000000000010", SizeGB: 50, DeleteOnTermination: true},
		{ImageID: "00000000-0000-0000-0000-000000000011", SizeGB: 100, DeleteOnTermination: true},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "block_devices", errInvalidBlockDeviceMapping)
}

func TestCreate_BlockDevices_SizeGB_Zero_Rejected(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.BlockDevices = []BlockDeviceMapping{
		{ImageID: "00000000-0000-0000-0000-000000000010", SizeGB: 0, DeleteOnTermination: true},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "block_devices[0].size_gb", errInvalidBlockDeviceMapping)
}

func TestCreate_BlockDevices_SizeGB_ExceedsShape_Rejected(t *testing.T) {
	// c1.small max disk is 50 GB; requesting 100 GB should fail.
	s := newTestSrv(t)
	body := validCreateBody() // c1.small
	body.BlockDevices = []BlockDeviceMapping{
		{ImageID: "00000000-0000-0000-0000-000000000010", SizeGB: 100, DeleteOnTermination: true},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "block_devices[0].size_gb", errInvalidBlockDeviceMapping)
}

func TestCreate_BlockDevices_SizeGB_MatchesShape_Accepted(t *testing.T) {
	// c1.medium max disk is 100 GB; requesting exactly 100 GB should succeed.
	s := newTestSrv(t)
	body := validCreateBody()
	body.InstanceType = "c1.medium"
	body.BlockDevices = []BlockDeviceMapping{
		{ImageID: "00000000-0000-0000-0000-000000000010", SizeGB: 100, DeleteOnTermination: true},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
}

func TestCreate_BlockDevices_SizeGB_BelowShape_Accepted(t *testing.T) {
	// c1.medium max disk is 100 GB; requesting 80 GB should succeed.
	s := newTestSrv(t)
	body := validCreateBody()
	body.InstanceType = "c1.medium"
	body.BlockDevices = []BlockDeviceMapping{
		{ImageID: "00000000-0000-0000-0000-000000000010", SizeGB: 80, DeleteOnTermination: true},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
}

func TestCreate_BlockDevices_ImageMismatch_Rejected(t *testing.T) {
	// block_devices[0].image_id must match the top-level image_id.
	s := newTestSrv(t)
	body := validCreateBody()
	body.ImageID = "00000000-0000-0000-0000-000000000010"
	body.BlockDevices = []BlockDeviceMapping{
		{ImageID: "00000000-0000-0000-0000-000000000011", SizeGB: 50, DeleteOnTermination: true},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}

	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "block_devices[0].image_id", errInvalidBlockDeviceMapping)
}

// ── M10 Slice 4: block_devices in GET responses ─────────────────────────────

func TestGetInstance_BlockDevices_WithRootDisk(t *testing.T) {
	// When a root_disk record exists, block_devices should reflect its values.
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_bd_get", "bd-get-test", alice, "running")

	instID := "inst_bd_get"
	s.mem.seedRootDisk(&db.RootDiskRow{
		DiskID:              "disk_inst_bd_get",
		InstanceID:          &instID,
		SourceImageID:       "00000000-0000-0000-0000-000000000010",
		StoragePoolID:       "pool_01",
		StoragePath:         "nfs://filer/vol/disk_inst_bd_get.qcow2",
		SizeGB:              50,
		DeleteOnTermination: true,
		Status:              db.RootDiskStatusAttached,
	})

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_bd_get", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var out InstanceResponse
	decodeBody(t, resp, &out)

	if len(out.BlockDevices) != 1 {
		t.Fatalf("want 1 block device, got %d", len(out.BlockDevices))
	}
	bd := out.BlockDevices[0]
	if bd.ImageID != "00000000-0000-0000-0000-000000000010" {
		t.Errorf("want image_id from root_disk, got %q", bd.ImageID)
	}
	if bd.SizeGB != 50 {
		t.Errorf("want size_gb=50, got %d", bd.SizeGB)
	}
	if !bd.DeleteOnTermination {
		t.Error("want delete_on_termination=true from root_disk")
	}
}

func TestGetInstance_BlockDevices_NoRootDisk_DefaultSynthesized(t *testing.T) {
	// Pre-Slice-4 instances have no root_disk record.
	// block_devices should be synthesized with Phase 1 defaults.
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_bd_nod", "bd-no-disk", alice, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_bd_nod", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var out InstanceResponse
	decodeBody(t, resp, &out)

	if len(out.BlockDevices) != 1 {
		t.Fatalf("want 1 block device (synthesized default), got %d", len(out.BlockDevices))
	}
	bd := out.BlockDevices[0]
	if !bd.DeleteOnTermination {
		t.Error("want delete_on_termination=true (Phase 1 default)")
	}
	if bd.SizeGB != 50 { // c1.small default
		t.Errorf("want size_gb=50, got %d", bd.SizeGB)
	}
}

func TestListInstances_BlockDevices_Present(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_list_bd", "list-bd-test", alice, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var out ListInstancesResponse
	decodeBody(t, resp, &out)

	if out.Total != 1 {
		t.Fatalf("want 1 instance, got %d", out.Total)
	}
	inst := out.Instances[0]
	if len(inst.BlockDevices) != 1 {
		t.Fatalf("want 1 block device in list response, got %d", len(inst.BlockDevices))
	}
	if !inst.BlockDevices[0].DeleteOnTermination {
		t.Error("want delete_on_termination=true")
	}
}

// ── M10 Slice 4: validation unit tests ──────────────────────────────────────

func TestValidation_BlockDevices_Phase1_DefaultTrue(t *testing.T) {
	// Verify that validateBlockDevices accepts delete_on_termination=true.
	req := validCreateBody()
	req.BlockDevices = []BlockDeviceMapping{
		{ImageID: req.ImageID, SizeGB: 50, DeleteOnTermination: true},
	}
	errs := validateCreateRequest(&req)
	for _, e := range errs {
		if e.target == "block_devices[0].delete_on_termination" {
			t.Errorf("delete_on_termination=true should be accepted, got error: %s", e.message)
		}
	}
}

func TestValidation_BlockDevices_Phase1_FalseRejected(t *testing.T) {
	req := validCreateBody()
	req.BlockDevices = []BlockDeviceMapping{
		{ImageID: req.ImageID, SizeGB: 50, DeleteOnTermination: false},
	}
	errs := validateCreateRequest(&req)
	found := false
	for _, e := range errs {
		if e.code == errDeleteOnTerminationRequired {
			found = true
			break
		}
	}
	if !found {
		t.Error("want delete_on_termination_required error for delete_on_termination=false")
	}
}

func TestCreate_BlockDevices_ResponseRoundTrip(t *testing.T) {
	// Verify block_devices from the create request appear in the create response.
	s := newTestSrv(t)
	body := validCreateBody()
	body.BlockDevices = []BlockDeviceMapping{
		{
			ImageID:             "00000000-0000-0000-0000-000000000010",
			SizeGB:              40,
			DeleteOnTermination: true,
		},
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	if len(out.Instance.BlockDevices) != 1 {
		t.Fatalf("want 1 block device in response, got %d", len(out.Instance.BlockDevices))
	}
	bd := out.Instance.BlockDevices[0]
	if bd.SizeGB != 40 {
		t.Errorf("want size_gb=40 from request, got %d", bd.SizeGB)
	}
	if bd.ImageID != "00000000-0000-0000-0000-000000000010" {
		t.Errorf("want image_id from request, got %q", bd.ImageID)
	}
	if !bd.DeleteOnTermination {
		t.Error("want delete_on_termination=true")
	}
}

// ── M10 Slice 4: Existing tests must still pass ─────────────────────────────
// (No changes needed — existing tests use validCreateBody() which omits
// block_devices. The handler synthesizes defaults, preserving behavior.)
