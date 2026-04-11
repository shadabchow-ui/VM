package db

// instance_networking_test.go — Unit tests for instance networking repo methods.
//
// M9 Slice 4: Tests cover the SQL logic and argument mapping for:
// - CreateNICSecurityGroupLink
// - ListSecurityGroupIDsByNIC
// - GetPrimaryNetworkInterfaceByInstance
// - AllocateIPFromSubnet
// - ReleaseIPFromSubnet
// - GetDefaultSecurityGroupForVPC
// - ValidateSecurityGroupsInVPC
// - SubnetContainsIP
// - GenerateMACAddress
//
// Uses the same fakePool/fakeRow/fakeRows pattern from repo_test.go.
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"fmt"
	"testing"
	"time"
)

// ── CreateNICSecurityGroupLink ───────────────────────────────────────────────

func TestCreateNICSecurityGroupLink_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.CreateNICSecurityGroupLink(ctx(), "nic_001", "sg_001"); err != nil {
		t.Fatalf("CreateNICSecurityGroupLink: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if len(call.args) < 2 {
		t.Fatalf("expected at least 2 args, got %d", len(call.args))
	}
	if call.args[0] != "nic_001" {
		t.Errorf("first arg = %v, want nic_001", call.args[0])
	}
	if call.args[1] != "sg_001" {
		t.Errorf("second arg = %v, want sg_001", call.args[1])
	}
}

func TestCreateNICSecurityGroupLink_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("fk violation")}
	r := newRepo(pool)

	err := r.CreateNICSecurityGroupLink(ctx(), "nic_001", "sg_missing")
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// ── ListSecurityGroupIDsByNIC ────────────────────────────────────────────────

func TestListSecurityGroupIDsByNIC_ReturnsMultipleSGs(t *testing.T) {
	pool := &fakePool{
		queryRowsData: [][]any{
			{"sg_001"},
			{"sg_002"},
			{"sg_003"},
		},
	}
	r := newRepo(pool)

	sgIDs, err := r.ListSecurityGroupIDsByNIC(ctx(), "nic_001")
	if err != nil {
		t.Fatalf("ListSecurityGroupIDsByNIC: %v", err)
	}
	if len(sgIDs) != 3 {
		t.Fatalf("expected 3 SG IDs, got %d", len(sgIDs))
	}
	if sgIDs[0] != "sg_001" {
		t.Errorf("first SG ID = %q, want sg_001", sgIDs[0])
	}
	if sgIDs[1] != "sg_002" {
		t.Errorf("second SG ID = %q, want sg_002", sgIDs[1])
	}
	if sgIDs[2] != "sg_003" {
		t.Errorf("third SG ID = %q, want sg_003", sgIDs[2])
	}
}

func TestListSecurityGroupIDsByNIC_ReturnsEmptySlice_WhenNoSGs(t *testing.T) {
	pool := &fakePool{queryRowsData: [][]any{}}
	r := newRepo(pool)

	sgIDs, err := r.ListSecurityGroupIDsByNIC(ctx(), "nic_no_sgs")
	if err != nil {
		t.Fatalf("ListSecurityGroupIDsByNIC: %v", err)
	}
	if len(sgIDs) != 0 {
		t.Errorf("expected empty slice, got %d items", len(sgIDs))
	}
}

// ── GetPrimaryNetworkInterfaceByInstance ─────────────────────────────────────

func TestGetPrimaryNetworkInterfaceByInstance_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"nic_001", "inst_001", "subnet_001", "vpc_001", "10.0.1.5",
			"02:00:00:00:00:01", 1, "attached", now, now, nil, // 1 for true (is_primary)
		}},
	}
	r := newRepo(pool)

	nic, err := r.GetPrimaryNetworkInterfaceByInstance(ctx(), "inst_001")
	if err != nil {
		t.Fatalf("GetPrimaryNetworkInterfaceByInstance: %v", err)
	}
	if nic.ID != "nic_001" {
		t.Errorf("ID = %q, want nic_001", nic.ID)
	}
	if nic.InstanceID != "inst_001" {
		t.Errorf("InstanceID = %q, want inst_001", nic.InstanceID)
	}
	if nic.SubnetID != "subnet_001" {
		t.Errorf("SubnetID = %q, want subnet_001", nic.SubnetID)
	}
	if nic.VPCID != "vpc_001" {
		t.Errorf("VPCID = %q, want vpc_001", nic.VPCID)
	}
	if nic.PrivateIP != "10.0.1.5" {
		t.Errorf("PrivateIP = %q, want 10.0.1.5", nic.PrivateIP)
	}
	if nic.Status != "attached" {
		t.Errorf("Status = %q, want attached", nic.Status)
	}
}

func TestGetPrimaryNetworkInterfaceByInstance_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	nic, err := r.GetPrimaryNetworkInterfaceByInstance(ctx(), "inst_classic")
	if err != nil {
		t.Fatalf("GetPrimaryNetworkInterfaceByInstance should not return error for missing row: %v", err)
	}
	if nic != nil {
		t.Errorf("expected nil NIC for Phase 1 classic instance, got %#v", nic)
	}
}

// ── AllocateIPFromSubnet ─────────────────────────────────────────────────────

func TestAllocateIPFromSubnet_ReturnsIP_OnSuccess(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{"10.0.1.5"}},
	}
	r := newRepo(pool)

	ip, err := r.AllocateIPFromSubnet(ctx(), "subnet_001", "inst_001")
	if err != nil {
		t.Fatalf("AllocateIPFromSubnet: %v", err)
	}
	if ip != "10.0.1.5" {
		t.Errorf("ip = %q, want 10.0.1.5", ip)
	}
}

func TestAllocateIPFromSubnet_ReturnsError_WhenSubnetExhausted(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.AllocateIPFromSubnet(ctx(), "subnet_exhausted", "inst_001")
	if err == nil {
		t.Error("expected error when subnet is exhausted, got nil")
	}
}

// ── ReleaseIPFromSubnet ──────────────────────────────────────────────────────

func TestReleaseIPFromSubnet_Success_IsIdempotent(t *testing.T) {
	// 0 rows affected is fine — already released.
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	if err := r.ReleaseIPFromSubnet(ctx(), "10.0.1.5", "subnet_001", "inst_001"); err != nil {
		t.Fatalf("ReleaseIPFromSubnet: %v", err)
	}
}

func TestReleaseIPFromSubnet_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.ReleaseIPFromSubnet(ctx(), "10.0.1.5", "subnet_001", "inst_001"); err != nil {
		t.Fatalf("ReleaseIPFromSubnet: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if len(call.args) < 3 {
		t.Fatalf("expected at least 3 args, got %d", len(call.args))
	}
	if call.args[0] != "10.0.1.5" {
		t.Errorf("first arg = %v, want 10.0.1.5", call.args[0])
	}
	if call.args[1] != "subnet_001" {
		t.Errorf("second arg = %v, want subnet_001", call.args[1])
	}
	if call.args[2] != "inst_001" {
		t.Errorf("third arg = %v, want inst_001", call.args[2])
	}
}

func TestReleaseIPFromSubnet_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("connection lost")}
	r := newRepo(pool)

	err := r.ReleaseIPFromSubnet(ctx(), "10.0.1.5", "subnet_001", "inst_001")
	if err == nil {
		t.Error("expected error from Exec, got nil")
	}
}

// ── GetDefaultSecurityGroupForVPC ────────────────────────────────────────────

func TestGetDefaultSecurityGroupForVPC_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"sg_default_001", "vpc_001", "princ_001", "default", nil, 1, now, now, nil, // 1 for true (is_default)
		}},
	}
	r := newRepo(pool)

	sg, err := r.GetDefaultSecurityGroupForVPC(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("GetDefaultSecurityGroupForVPC: %v", err)
	}
	if sg.ID != "sg_default_001" {
		t.Errorf("ID = %q, want sg_default_001", sg.ID)
	}
	if sg.VPCID != "vpc_001" {
		t.Errorf("VPCID = %q, want vpc_001", sg.VPCID)
	}
	if sg.Name != "default" {
		t.Errorf("Name = %q, want default", sg.Name)
	}
}

func TestGetDefaultSecurityGroupForVPC_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	sg, err := r.GetDefaultSecurityGroupForVPC(ctx(), "vpc_no_default")
	if err != nil {
		t.Fatalf("GetDefaultSecurityGroupForVPC should not return error for missing row: %v", err)
	}
	if sg != nil {
		t.Errorf("expected nil SG for VPC without default, got %#v", sg)
	}
}

// ── ValidateSecurityGroupsInVPC ──────────────────────────────────────────────
// These tests require GetSecurityGroupByID to be called, which uses QueryRow.
// We simulate different scenarios by changing queryRowResult between calls.

func TestValidateSecurityGroupsInVPC_ReturnsNil_WhenAllValid(t *testing.T) {
	now := time.Now()
	// Mock returns a valid SG in the correct VPC owned by the principal
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"sg_001", "vpc_001", "princ_001", "my-sg", nil, 0, now, now, nil, // 0 for false (not default)
		}},
	}
	r := newRepo(pool)

	err := r.ValidateSecurityGroupsInVPC(ctx(), []string{"sg_001"}, "vpc_001", "princ_001")
	if err != nil {
		t.Errorf("expected nil error for valid SG, got %v", err)
	}
}

func TestValidateSecurityGroupsInVPC_ReturnsError_WhenSGNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	err := r.ValidateSecurityGroupsInVPC(ctx(), []string{"sg_missing"}, "vpc_001", "princ_001")
	if err == nil {
		t.Error("expected error for missing SG, got nil")
	}
}

func TestValidateSecurityGroupsInVPC_ReturnsError_WhenSGInWrongVPC(t *testing.T) {
	now := time.Now()
	// Mock returns a SG in a different VPC
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"sg_other", "vpc_other", "princ_001", "other-sg", nil, 0, now, now, nil,
		}},
	}
	r := newRepo(pool)

	err := r.ValidateSecurityGroupsInVPC(ctx(), []string{"sg_other"}, "vpc_001", "princ_001")
	if err == nil {
		t.Error("expected error for SG in wrong VPC, got nil")
	}
}

func TestValidateSecurityGroupsInVPC_AllowsDefaultSG_EvenIfNotOwned(t *testing.T) {
	now := time.Now()
	// Mock returns a default SG owned by someone else
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"sg_default", "vpc_001", "princ_other", "default", nil, 1, now, now, nil, // 1 = is_default
		}},
	}
	r := newRepo(pool)

	err := r.ValidateSecurityGroupsInVPC(ctx(), []string{"sg_default"}, "vpc_001", "princ_001")
	if err != nil {
		t.Errorf("expected nil error for default SG (even if not owned), got %v", err)
	}
}

func TestValidateSecurityGroupsInVPC_ReturnsError_WhenNotOwnedAndNotDefault(t *testing.T) {
	now := time.Now()
	// Mock returns a non-default SG owned by someone else
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"sg_private", "vpc_001", "princ_other", "private-sg", nil, 0, now, now, nil, // 0 = not default
		}},
	}
	r := newRepo(pool)

	err := r.ValidateSecurityGroupsInVPC(ctx(), []string{"sg_private"}, "vpc_001", "princ_001")
	if err == nil {
		t.Error("expected error for non-owned non-default SG, got nil")
	}
}

func TestValidateSecurityGroupsInVPC_ReturnsNil_WhenEmptyList(t *testing.T) {
	pool := &fakePool{}
	r := newRepo(pool)

	err := r.ValidateSecurityGroupsInVPC(ctx(), []string{}, "vpc_001", "princ_001")
	if err != nil {
		t.Errorf("expected nil error for empty SG list, got %v", err)
	}
}

// ── SubnetContainsIP ─────────────────────────────────────────────────────────

func TestSubnetContainsIP_ReturnsTrue_WhenIPInCIDR(t *testing.T) {
	contains, err := SubnetContainsIP("10.0.1.0/24", "10.0.1.50")
	if err != nil {
		t.Fatalf("SubnetContainsIP: %v", err)
	}
	if !contains {
		t.Error("expected 10.0.1.50 to be in 10.0.1.0/24")
	}
}

func TestSubnetContainsIP_ReturnsFalse_WhenIPOutsideCIDR(t *testing.T) {
	contains, err := SubnetContainsIP("10.0.1.0/24", "10.0.2.50")
	if err != nil {
		t.Fatalf("SubnetContainsIP: %v", err)
	}
	if contains {
		t.Error("expected 10.0.2.50 to NOT be in 10.0.1.0/24")
	}
}

func TestSubnetContainsIP_ReturnsError_WhenInvalidCIDR(t *testing.T) {
	_, err := SubnetContainsIP("not-a-cidr", "10.0.1.50")
	if err == nil {
		t.Error("expected error for invalid CIDR, got nil")
	}
}

func TestSubnetContainsIP_ReturnsError_WhenInvalidIP(t *testing.T) {
	_, err := SubnetContainsIP("10.0.1.0/24", "not-an-ip")
	if err == nil {
		t.Error("expected error for invalid IP, got nil")
	}
}

// ── GenerateMACAddress ───────────────────────────────────────────────────────

func TestGenerateMACAddress_ReturnsLocallyAdministeredMAC(t *testing.T) {
	mac := GenerateMACAddress()
	if mac == "" {
		t.Error("expected non-empty MAC address")
	}
	// Should start with 02: (locally administered)
	if len(mac) < 3 || mac[:3] != "02:" {
		t.Errorf("MAC should start with '02:', got %q", mac)
	}
}
