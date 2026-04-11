package db

// host_networking_test.go — Unit tests for cross-host networking repo methods.
//
// M9 Slice 4: Tests for host_tunnel_endpoints, vpc_host_attachments,
// vni_allocations, and nic_vtep_registrations repo methods using fakePool.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"fmt"
	"testing"
	"time"
)

// ── HostTunnelEndpoint Tests ─────────────────────────────────────────────────

func TestUpsertHostTunnelEndpoint_Insert_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	mac := "02:00:00:00:00:01"
	row := &HostTunnelEndpointRow{
		HostID:        "host_001",
		VTEPIP:        "10.200.0.1",
		VTEPMAC:       &mac,
		VTEPInterface: "vxlan0",
		Status:        "active",
	}

	if err := r.UpsertHostTunnelEndpoint(ctx(), row); err != nil {
		t.Fatalf("UpsertHostTunnelEndpoint: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "host_001" {
		t.Errorf("first arg = %v, want host_001", call.args[0])
	}
	if call.args[1] != "10.200.0.1" {
		t.Errorf("second arg = %v, want 10.200.0.1", call.args[1])
	}
	if call.args[4] != "active" {
		t.Errorf("fifth arg = %v, want active", call.args[4])
	}
}

func TestUpsertHostTunnelEndpoint_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db error")}
	r := newRepo(pool)

	err := r.UpsertHostTunnelEndpoint(ctx(), &HostTunnelEndpointRow{HostID: "host_x"})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestGetHostTunnelEndpoint_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	mac := "02:00:00:00:00:01"
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"host_001", "10.200.0.1", &mac, "vxlan0", "active", now, now,
		}},
	}
	r := newRepo(pool)

	hte, err := r.GetHostTunnelEndpoint(ctx(), "host_001")
	if err != nil {
		t.Fatalf("GetHostTunnelEndpoint: %v", err)
	}
	if hte.HostID != "host_001" {
		t.Errorf("HostID = %q, want host_001", hte.HostID)
	}
	if hte.VTEPIP != "10.200.0.1" {
		t.Errorf("VTEPIP = %q, want 10.200.0.1", hte.VTEPIP)
	}
	if hte.Status != "active" {
		t.Errorf("Status = %q, want active", hte.Status)
	}
}

func TestGetHostTunnelEndpoint_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	hte, err := r.GetHostTunnelEndpoint(ctx(), "host_missing")
	if err != nil {
		t.Fatalf("GetHostTunnelEndpoint should not return error for missing row: %v", err)
	}
	if hte != nil {
		t.Errorf("expected nil for missing host, got %#v", hte)
	}
}

func TestListActiveHostTunnelEndpoints_ReturnsMultiple(t *testing.T) {
	now := time.Now()
	mac := "02:00:00:00:00:01"
	pool := &fakePool{
		queryRowsData: [][]any{
			{"host_001", "10.200.0.1", &mac, "vxlan0", "active", now, now},
			{"host_002", "10.200.0.2", &mac, "vxlan0", "active", now, now},
		},
	}
	r := newRepo(pool)

	htes, err := r.ListActiveHostTunnelEndpoints(ctx())
	if err != nil {
		t.Fatalf("ListActiveHostTunnelEndpoints: %v", err)
	}
	if len(htes) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(htes))
	}
	if htes[0].HostID != "host_001" {
		t.Errorf("first HostID = %q, want host_001", htes[0].HostID)
	}
	if htes[1].HostID != "host_002" {
		t.Errorf("second HostID = %q, want host_002", htes[1].HostID)
	}
}

func TestUpdateHostTunnelEndpointStatus_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.UpdateHostTunnelEndpointStatus(ctx(), "host_001", "draining"); err != nil {
		t.Fatalf("UpdateHostTunnelEndpointStatus: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
}

func TestUpdateHostTunnelEndpointStatus_ReturnsError_WhenNotFound(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.UpdateHostTunnelEndpointStatus(ctx(), "host_missing", "draining")
	if err == nil {
		t.Error("expected error for missing host, got nil")
	}
}

// ── VPCHostAttachment Tests ──────────────────────────────────────────────────

func TestIncrementVPCHostAttachment_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.IncrementVPCHostAttachment(ctx(), "vha_001", "vpc_001", "host_001"); err != nil {
		t.Fatalf("IncrementVPCHostAttachment: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "vha_001" {
		t.Errorf("first arg = %v, want vha_001", call.args[0])
	}
	if call.args[1] != "vpc_001" {
		t.Errorf("second arg = %v, want vpc_001", call.args[1])
	}
	if call.args[2] != "host_001" {
		t.Errorf("third arg = %v, want host_001", call.args[2])
	}
}

func TestDecrementVPCHostAttachment_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.DecrementVPCHostAttachment(ctx(), "vpc_001", "host_001"); err != nil {
		t.Fatalf("DecrementVPCHostAttachment: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "vpc_001" {
		t.Errorf("first arg = %v, want vpc_001", call.args[0])
	}
	if call.args[1] != "host_001" {
		t.Errorf("second arg = %v, want host_001", call.args[1])
	}
}

func TestGetVPCHostAttachment_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"vha_001", "vpc_001", "host_001", 3, now, now,
		}},
	}
	r := newRepo(pool)

	vha, err := r.GetVPCHostAttachment(ctx(), "vpc_001", "host_001")
	if err != nil {
		t.Fatalf("GetVPCHostAttachment: %v", err)
	}
	if vha.ID != "vha_001" {
		t.Errorf("ID = %q, want vha_001", vha.ID)
	}
	if vha.InstanceCount != 3 {
		t.Errorf("InstanceCount = %d, want 3", vha.InstanceCount)
	}
}

func TestGetVPCHostAttachment_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	vha, err := r.GetVPCHostAttachment(ctx(), "vpc_missing", "host_missing")
	if err != nil {
		t.Fatalf("GetVPCHostAttachment should not return error for missing row: %v", err)
	}
	if vha != nil {
		t.Errorf("expected nil for missing attachment, got %#v", vha)
	}
}

func TestListHostsInVPC_ReturnsMultiple(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"vha_001", "vpc_001", "host_001", 2, now, now},
			{"vha_002", "vpc_001", "host_002", 1, now, now},
		},
	}
	r := newRepo(pool)

	attachments, err := r.ListHostsInVPC(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("ListHostsInVPC: %v", err)
	}
	if len(attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(attachments))
	}
	if attachments[0].HostID != "host_001" {
		t.Errorf("first HostID = %q, want host_001", attachments[0].HostID)
	}
}

func TestListVPCsOnHost_ReturnsMultiple(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"vha_001", "vpc_001", "host_001", 2, now, now},
			{"vha_002", "vpc_002", "host_001", 1, now, now},
		},
	}
	r := newRepo(pool)

	attachments, err := r.ListVPCsOnHost(ctx(), "host_001")
	if err != nil {
		t.Fatalf("ListVPCsOnHost: %v", err)
	}
	if len(attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(attachments))
	}
	if attachments[0].VPCID != "vpc_001" {
		t.Errorf("first VPCID = %q, want vpc_001", attachments[0].VPCID)
	}
}

// ── VNI Allocation Tests ─────────────────────────────────────────────────────

func TestAllocateVNI_ReturnsVNI_WhenAvailable(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{4096}},
	}
	r := newRepo(pool)

	vni, err := r.AllocateVNI(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("AllocateVNI: %v", err)
	}
	if vni != 4096 {
		t.Errorf("vni = %d, want 4096", vni)
	}
}

func TestAllocateVNI_ReturnsError_WhenExhausted(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.AllocateVNI(ctx(), "vpc_001")
	if err == nil {
		t.Error("expected error when VNI pool exhausted, got nil")
	}
}

func TestReleaseVNI_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.ReleaseVNI(ctx(), 4096); err != nil {
		t.Fatalf("ReleaseVNI: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != 4096 {
		t.Errorf("first arg = %v, want 4096", call.args[0])
	}
}

func TestGetVNIByVPC_ReturnsVNI_WhenAllocated(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{4097}},
	}
	r := newRepo(pool)

	vni, err := r.GetVNIByVPC(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("GetVNIByVPC: %v", err)
	}
	if vni != 4097 {
		t.Errorf("vni = %d, want 4097", vni)
	}
}

func TestGetVNIByVPC_ReturnsZero_WhenNotAllocated(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	vni, err := r.GetVNIByVPC(ctx(), "vpc_missing")
	if err != nil {
		t.Fatalf("GetVNIByVPC should not return error for unallocated VPC: %v", err)
	}
	if vni != 0 {
		t.Errorf("vni = %d, want 0 for unallocated VPC", vni)
	}
}

// ── NIC VTEP Registration Tests ──────────────────────────────────────────────

func TestCreateNICVTEPRegistration_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	row := &NICVTEPRegistrationRow{
		ID:         "nvr_001",
		NICID:      "nic_001",
		VPCID:      "vpc_001",
		HostID:     "host_001",
		PrivateIP:  "10.0.1.5",
		MACAddress: "02:00:00:00:00:05",
		VNI:        4096,
		Status:     "active",
	}

	if err := r.CreateNICVTEPRegistration(ctx(), row); err != nil {
		t.Fatalf("CreateNICVTEPRegistration: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "nvr_001" {
		t.Errorf("first arg = %v, want nvr_001", call.args[0])
	}
	if call.args[1] != "nic_001" {
		t.Errorf("second arg = %v, want nic_001", call.args[1])
	}
	if call.args[6] != 4096 {
		t.Errorf("seventh arg = %v, want 4096", call.args[6])
	}
}

func TestGetNICVTEPRegistration_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"nvr_001", "nic_001", "vpc_001", "host_001", "10.0.1.5",
			"02:00:00:00:00:05", 4096, "active", now, now,
		}},
	}
	r := newRepo(pool)

	nvr, err := r.GetNICVTEPRegistration(ctx(), "nvr_001")
	if err != nil {
		t.Fatalf("GetNICVTEPRegistration: %v", err)
	}
	if nvr.ID != "nvr_001" {
		t.Errorf("ID = %q, want nvr_001", nvr.ID)
	}
	if nvr.VNI != 4096 {
		t.Errorf("VNI = %d, want 4096", nvr.VNI)
	}
	if nvr.Status != "active" {
		t.Errorf("Status = %q, want active", nvr.Status)
	}
}

func TestGetNICVTEPRegistration_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	nvr, err := r.GetNICVTEPRegistration(ctx(), "nvr_missing")
	if err != nil {
		t.Fatalf("GetNICVTEPRegistration should not return error for missing row: %v", err)
	}
	if nvr != nil {
		t.Errorf("expected nil for missing registration, got %#v", nvr)
	}
}

func TestGetNICVTEPRegistrationByNIC_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"nvr_001", "nic_001", "vpc_001", "host_001", "10.0.1.5",
			"02:00:00:00:00:05", 4096, "active", now, now,
		}},
	}
	r := newRepo(pool)

	nvr, err := r.GetNICVTEPRegistrationByNIC(ctx(), "nic_001")
	if err != nil {
		t.Fatalf("GetNICVTEPRegistrationByNIC: %v", err)
	}
	if nvr.NICID != "nic_001" {
		t.Errorf("NICID = %q, want nic_001", nvr.NICID)
	}
}

func TestListNICVTEPRegistrationsByVPC_ReturnsMultiple(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"nvr_001", "nic_001", "vpc_001", "host_001", "10.0.1.5", "02:00:00:00:00:05", 4096, "active", now, now},
			{"nvr_002", "nic_002", "vpc_001", "host_002", "10.0.1.6", "02:00:00:00:00:06", 4096, "active", now, now},
		},
	}
	r := newRepo(pool)

	regs, err := r.ListNICVTEPRegistrationsByVPC(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("ListNICVTEPRegistrationsByVPC: %v", err)
	}
	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations, got %d", len(regs))
	}
	if regs[0].ID != "nvr_001" {
		t.Errorf("first ID = %q, want nvr_001", regs[0].ID)
	}
}

func TestListNICVTEPRegistrationsByHost_ReturnsMultiple(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"nvr_001", "nic_001", "vpc_001", "host_001", "10.0.1.5", "02:00:00:00:00:05", 4096, "active", now, now},
			{"nvr_002", "nic_002", "vpc_002", "host_001", "10.1.1.5", "02:00:00:00:00:07", 4097, "active", now, now},
		},
	}
	r := newRepo(pool)

	regs, err := r.ListNICVTEPRegistrationsByHost(ctx(), "host_001")
	if err != nil {
		t.Fatalf("ListNICVTEPRegistrationsByHost: %v", err)
	}
	if len(regs) != 2 {
		t.Fatalf("expected 2 registrations, got %d", len(regs))
	}
	if regs[0].HostID != "host_001" {
		t.Errorf("first HostID = %q, want host_001", regs[0].HostID)
	}
}

func TestUpdateNICVTEPRegistrationStatus_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.UpdateNICVTEPRegistrationStatus(ctx(), "nvr_001", "removed"); err != nil {
		t.Fatalf("UpdateNICVTEPRegistrationStatus: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
}

func TestUpdateNICVTEPRegistrationStatus_ReturnsError_WhenNotFound(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.UpdateNICVTEPRegistrationStatus(ctx(), "nvr_missing", "removed")
	if err == nil {
		t.Error("expected error for missing registration, got nil")
	}
}

func TestLookupVTEPForPrivateIP_ReturnsVTEPIP_WhenFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{"10.200.0.1"}},
	}
	r := newRepo(pool)

	vtepIP, err := r.LookupVTEPForPrivateIP(ctx(), "vpc_001", "10.0.1.5")
	if err != nil {
		t.Fatalf("LookupVTEPForPrivateIP: %v", err)
	}
	if vtepIP != "10.200.0.1" {
		t.Errorf("vtepIP = %q, want 10.200.0.1", vtepIP)
	}
}

func TestLookupVTEPForPrivateIP_ReturnsError_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.LookupVTEPForPrivateIP(ctx(), "vpc_001", "10.0.1.99")
	if err == nil {
		t.Error("expected error when VTEP not found, got nil")
	}
}
