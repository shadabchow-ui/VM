package db

// network_repo_test.go — Unit tests for VPC, Subnet, SecurityGroup, SecurityGroupRule,
// and NetworkInterface repo methods using fakePool.
//
// Tests cover the SQL logic and argument mapping for all network repo methods
// without requiring a real PostgreSQL instance.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.
// Phase 2 M9: Private networking foundation persistence tests.

import (
	"fmt"
	"testing"
	"time"
)

// ── VPC Tests ─────────────────────────────────────────────────────────────────

func TestCreateVPC_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	row := &VPCRow{
		ID:               "vpc_test001",
		OwnerPrincipalID: "princ_001",
		Name:             "my-vpc",
		CIDRIPv4:         "10.0.0.0/16",
		Status:           "available",
	}

	if err := r.CreateVPC(ctx(), row); err != nil {
		t.Fatalf("CreateVPC: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if len(call.args) < 5 {
		t.Fatalf("expected at least 5 args, got %d", len(call.args))
	}
	if call.args[0] != "vpc_test001" {
		t.Errorf("first arg = %v, want vpc_test001", call.args[0])
	}
	if call.args[1] != "princ_001" {
		t.Errorf("second arg = %v, want princ_001", call.args[1])
	}
	if call.args[2] != "my-vpc" {
		t.Errorf("third arg = %v, want my-vpc", call.args[2])
	}
	if call.args[3] != "10.0.0.0/16" {
		t.Errorf("fourth arg = %v, want 10.0.0.0/16", call.args[3])
	}
	if call.args[4] != "available" {
		t.Errorf("fifth arg = %v, want available", call.args[4])
	}
}

func TestCreateVPC_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db error")}
	r := newRepo(pool)

	err := r.CreateVPC(ctx(), &VPCRow{ID: "vpc_x"})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestGetVPCByID_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"vpc_001", "princ_001", "my-vpc", "10.0.0.0/16",
			"available", now, now, nil,
		}},
	}
	r := newRepo(pool)

	vpc, err := r.GetVPCByID(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("GetVPCByID: %v", err)
	}
	if vpc.ID != "vpc_001" {
		t.Errorf("ID = %q, want vpc_001", vpc.ID)
	}
	if vpc.Name != "my-vpc" {
		t.Errorf("Name = %q, want my-vpc", vpc.Name)
	}
	if vpc.Status != "available" {
		t.Errorf("Status = %q, want available", vpc.Status)
	}
}

func TestGetVPCByID_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	vpc, err := r.GetVPCByID(ctx(), "vpc_missing")
	if err != nil {
		t.Fatalf("GetVPCByID should not return error for missing row: %v", err)
	}
	if vpc != nil {
		t.Errorf("expected nil VPC for missing id, got %#v", vpc)
	}
}

func TestListVPCsByOwner_ReturnsEmptySlice_WhenNoVPCs(t *testing.T) {
	pool := &fakePool{queryRowsData: [][]any{}}
	r := newRepo(pool)

	vpcs, err := r.ListVPCsByOwner(ctx(), "princ_001")
	if err != nil {
		t.Fatalf("ListVPCsByOwner: %v", err)
	}
	if len(vpcs) != 0 {
		t.Errorf("expected empty slice, got %d items", len(vpcs))
	}
}

func TestListVPCsByOwner_ReturnsMultipleVPCs(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"vpc_001", "princ_001", "vpc-one", "10.0.0.0/16", "available", now, now, nil},
			{"vpc_002", "princ_001", "vpc-two", "10.1.0.0/16", "available", now, now, nil},
		},
	}
	r := newRepo(pool)

	vpcs, err := r.ListVPCsByOwner(ctx(), "princ_001")
	if err != nil {
		t.Fatalf("ListVPCsByOwner: %v", err)
	}
	if len(vpcs) != 2 {
		t.Fatalf("expected 2 VPCs, got %d", len(vpcs))
	}
	if vpcs[0].ID != "vpc_001" {
		t.Errorf("first VPC ID = %q, want vpc_001", vpcs[0].ID)
	}
	if vpcs[1].ID != "vpc_002" {
		t.Errorf("second VPC ID = %q, want vpc_002", vpcs[1].ID)
	}
}

func TestSoftDeleteVPC_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.SoftDeleteVPC(ctx(), "vpc_001"); err != nil {
		t.Fatalf("SoftDeleteVPC: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
}

func TestSoftDeleteVPC_ReturnsError_WhenNotFound(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.SoftDeleteVPC(ctx(), "vpc_missing")
	if err == nil {
		t.Error("expected error for missing VPC, got nil")
	}
}

// ── Subnet Tests ──────────────────────────────────────────────────────────────

func TestCreateSubnet_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	row := &SubnetRow{
		ID:               "subnet_test001",
		VPCID:            "vpc_001",
		Name:             "my-subnet",
		CIDRIPv4:         "10.0.1.0/24",
		AvailabilityZone: "us-east-1a",
		Status:           "available",
	}

	if err := r.CreateSubnet(ctx(), row); err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if len(call.args) < 6 {
		t.Fatalf("expected at least 6 args, got %d", len(call.args))
	}
	if call.args[0] != "subnet_test001" {
		t.Errorf("first arg = %v, want subnet_test001", call.args[0])
	}
	if call.args[1] != "vpc_001" {
		t.Errorf("second arg = %v, want vpc_001", call.args[1])
	}
}

func TestCreateSubnet_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("fk violation")}
	r := newRepo(pool)

	err := r.CreateSubnet(ctx(), &SubnetRow{ID: "subnet_x", VPCID: "vpc_missing"})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestGetSubnetByID_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"subnet_001", "vpc_001", "my-subnet", "10.0.1.0/24",
			"us-east-1a", "available", now, now, nil,
		}},
	}
	r := newRepo(pool)

	subnet, err := r.GetSubnetByID(ctx(), "subnet_001")
	if err != nil {
		t.Fatalf("GetSubnetByID: %v", err)
	}
	if subnet.ID != "subnet_001" {
		t.Errorf("ID = %q, want subnet_001", subnet.ID)
	}
	if subnet.VPCID != "vpc_001" {
		t.Errorf("VPCID = %q, want vpc_001", subnet.VPCID)
	}
}

func TestGetSubnetByID_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	subnet, err := r.GetSubnetByID(ctx(), "subnet_missing")
	if err != nil {
		t.Fatalf("GetSubnetByID should not return error for missing row: %v", err)
	}
	if subnet != nil {
		t.Errorf("expected nil Subnet for missing id, got %#v", subnet)
	}
}

func TestListSubnetsByVPC_ReturnsMultipleSubnets(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"subnet_001", "vpc_001", "subnet-one", "10.0.1.0/24", "us-east-1a", "available", now, now, nil},
			{"subnet_002", "vpc_001", "subnet-two", "10.0.2.0/24", "us-east-1b", "available", now, now, nil},
		},
	}
	r := newRepo(pool)

	subnets, err := r.ListSubnetsByVPC(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("ListSubnetsByVPC: %v", err)
	}
	if len(subnets) != 2 {
		t.Fatalf("expected 2 subnets, got %d", len(subnets))
	}
}

func TestSoftDeleteSubnet_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.SoftDeleteSubnet(ctx(), "subnet_001"); err != nil {
		t.Fatalf("SoftDeleteSubnet: %v", err)
	}
}

func TestSoftDeleteSubnet_ReturnsError_WhenNotFound(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.SoftDeleteSubnet(ctx(), "subnet_missing")
	if err == nil {
		t.Error("expected error for missing Subnet, got nil")
	}
}

// ── SecurityGroup Tests ───────────────────────────────────────────────────────

func TestCreateSecurityGroup_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	desc := "test security group"
	row := &SecurityGroupRow{
		ID:               "sg_test001",
		VPCID:            "vpc_001",
		OwnerPrincipalID: "princ_001",
		Name:             "my-sg",
		Description:      &desc,
		IsDefault:        false,
	}

	if err := r.CreateSecurityGroup(ctx(), row); err != nil {
		t.Fatalf("CreateSecurityGroup: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "sg_test001" {
		t.Errorf("first arg = %v, want sg_test001", call.args[0])
	}
	if call.args[1] != "vpc_001" {
		t.Errorf("second arg = %v, want vpc_001", call.args[1])
	}
	if call.args[2] != "princ_001" {
		t.Errorf("third arg = %v, want princ_001", call.args[2])
	}
	if call.args[3] != "my-sg" {
		t.Errorf("fourth arg = %v, want my-sg", call.args[3])
	}
}

func TestCreateSecurityGroup_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db error")}
	r := newRepo(pool)

	err := r.CreateSecurityGroup(ctx(), &SecurityGroupRow{ID: "sg_x"})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestGetSecurityGroupByID_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	desc := "default sg"
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"sg_001", "vpc_001", "princ_001", "default", desc, 1, // 1 for true
			now, now, nil,
		}},
	}
	r := newRepo(pool)

	sg, err := r.GetSecurityGroupByID(ctx(), "sg_001")
	if err != nil {
		t.Fatalf("GetSecurityGroupByID: %v", err)
	}
	if sg.ID != "sg_001" {
		t.Errorf("ID = %q, want sg_001", sg.ID)
	}
	if sg.VPCID != "vpc_001" {
		t.Errorf("VPCID = %q, want vpc_001", sg.VPCID)
	}
	if sg.Name != "default" {
		t.Errorf("Name = %q, want default", sg.Name)
	}
}

func TestGetSecurityGroupByID_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	sg, err := r.GetSecurityGroupByID(ctx(), "sg_missing")
	if err != nil {
		t.Fatalf("GetSecurityGroupByID should not return error for missing row: %v", err)
	}
	if sg != nil {
		t.Errorf("expected nil SecurityGroup for missing id, got %#v", sg)
	}
}

func TestListSecurityGroupsByVPC_ReturnsMultipleSGs(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"sg_001", "vpc_001", "princ_001", "default", nil, 1, now, now, nil},
			{"sg_002", "vpc_001", "princ_001", "custom", nil, 0, now, now, nil},
		},
	}
	r := newRepo(pool)

	sgs, err := r.ListSecurityGroupsByVPC(ctx(), "vpc_001")
	if err != nil {
		t.Fatalf("ListSecurityGroupsByVPC: %v", err)
	}
	if len(sgs) != 2 {
		t.Fatalf("expected 2 security groups, got %d", len(sgs))
	}
	if sgs[0].ID != "sg_001" {
		t.Errorf("first SG ID = %q, want sg_001", sgs[0].ID)
	}
}

// ── SecurityGroupRule Tests ───────────────────────────────────────────────────

func TestCreateSecurityGroupRule_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	portFrom := 22
	portTo := 22
	cidr := "0.0.0.0/0"
	row := &SecurityGroupRuleRow{
		ID:              "sgr_test001",
		SecurityGroupID: "sg_001",
		Direction:       "ingress",
		Protocol:        "tcp",
		PortFrom:        &portFrom,
		PortTo:          &portTo,
		CIDR:            &cidr,
	}

	if err := r.CreateSecurityGroupRule(ctx(), row); err != nil {
		t.Fatalf("CreateSecurityGroupRule: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "sgr_test001" {
		t.Errorf("first arg = %v, want sgr_test001", call.args[0])
	}
	if call.args[1] != "sg_001" {
		t.Errorf("second arg = %v, want sg_001", call.args[1])
	}
	if call.args[2] != "ingress" {
		t.Errorf("third arg = %v, want ingress", call.args[2])
	}
	if call.args[3] != "tcp" {
		t.Errorf("fourth arg = %v, want tcp", call.args[3])
	}
}

func TestCreateSecurityGroupRule_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("fk violation")}
	r := newRepo(pool)

	err := r.CreateSecurityGroupRule(ctx(), &SecurityGroupRuleRow{ID: "sgr_x", SecurityGroupID: "sg_missing"})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestDeleteSecurityGroupRule_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.DeleteSecurityGroupRule(ctx(), "sgr_001"); err != nil {
		t.Fatalf("DeleteSecurityGroupRule: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
}

func TestDeleteSecurityGroupRule_ReturnsError_WhenNotFound(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.DeleteSecurityGroupRule(ctx(), "sgr_missing")
	if err == nil {
		t.Error("expected error for missing rule, got nil")
	}
}

func TestListSecurityGroupRulesBySecurityGroup_ReturnsMultipleRules(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"sgr_001", "sg_001", "ingress", "tcp", 22, 22, "0.0.0.0/0", nil, now},
			{"sgr_002", "sg_001", "egress", "all", nil, nil, "0.0.0.0/0", nil, now},
		},
	}
	r := newRepo(pool)

	rules, err := r.ListSecurityGroupRulesBySecurityGroup(ctx(), "sg_001")
	if err != nil {
		t.Fatalf("ListSecurityGroupRulesBySecurityGroup: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if rules[0].ID != "sgr_001" {
		t.Errorf("first rule ID = %q, want sgr_001", rules[0].ID)
	}
	if rules[0].Direction != "ingress" {
		t.Errorf("first rule Direction = %q, want ingress", rules[0].Direction)
	}
	if rules[1].Direction != "egress" {
		t.Errorf("second rule Direction = %q, want egress", rules[1].Direction)
	}
}

// ── NetworkInterface Tests ────────────────────────────────────────────────────

func TestCreateNetworkInterface_CallsExecWithCorrectArgs(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	row := &NetworkInterfaceRow{
		ID:         "nic_test001",
		InstanceID: "inst_001",
		SubnetID:   "subnet_001",
		VPCID:      "vpc_001",
		PrivateIP:  "10.0.1.5",
		MACAddress: "02:00:00:00:00:01",
		IsPrimary:  true,
		Status:     "attached",
	}

	if err := r.CreateNetworkInterface(ctx(), row); err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}

	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "nic_test001" {
		t.Errorf("first arg = %v, want nic_test001", call.args[0])
	}
	if call.args[1] != "inst_001" {
		t.Errorf("second arg = %v, want inst_001", call.args[1])
	}
	if call.args[2] != "subnet_001" {
		t.Errorf("third arg = %v, want subnet_001", call.args[2])
	}
	if call.args[3] != "vpc_001" {
		t.Errorf("fourth arg = %v, want vpc_001", call.args[3])
	}
	if call.args[4] != "10.0.1.5" {
		t.Errorf("fifth arg = %v, want 10.0.1.5", call.args[4])
	}
	if call.args[5] != "02:00:00:00:00:01" {
		t.Errorf("sixth arg = %v, want 02:00:00:00:00:01", call.args[5])
	}
}

func TestCreateNetworkInterface_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("fk violation")}
	r := newRepo(pool)

	err := r.CreateNetworkInterface(ctx(), &NetworkInterfaceRow{ID: "nic_x", SubnetID: "subnet_missing"})
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestGetNetworkInterfaceByID_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"nic_001", "inst_001", "subnet_001", "vpc_001", "10.0.1.5",
			"02:00:00:00:00:01", 1, "attached", now, now, nil, // 1 for true
		}},
	}
	r := newRepo(pool)

	nic, err := r.GetNetworkInterfaceByID(ctx(), "nic_001")
	if err != nil {
		t.Fatalf("GetNetworkInterfaceByID: %v", err)
	}
	if nic.ID != "nic_001" {
		t.Errorf("ID = %q, want nic_001", nic.ID)
	}
	if nic.InstanceID != "inst_001" {
		t.Errorf("InstanceID = %q, want inst_001", nic.InstanceID)
	}
	if nic.PrivateIP != "10.0.1.5" {
		t.Errorf("PrivateIP = %q, want 10.0.1.5", nic.PrivateIP)
	}
	if nic.Status != "attached" {
		t.Errorf("Status = %q, want attached", nic.Status)
	}
}

func TestGetNetworkInterfaceByID_ReturnsNil_WhenNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	nic, err := r.GetNetworkInterfaceByID(ctx(), "nic_missing")
	if err != nil {
		t.Fatalf("GetNetworkInterfaceByID should not return error for missing row: %v", err)
	}
	if nic != nil {
		t.Errorf("expected nil NetworkInterface for missing id, got %#v", nic)
	}
}

func TestListNetworkInterfacesByInstance_ReturnsMultipleNICs(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"nic_001", "inst_001", "subnet_001", "vpc_001", "10.0.1.5", "02:00:00:00:00:01", 1, "attached", now, now, nil},
			{"nic_002", "inst_001", "subnet_002", "vpc_001", "10.0.2.5", "02:00:00:00:00:02", 0, "attached", now, now, nil},
		},
	}
	r := newRepo(pool)

	nics, err := r.ListNetworkInterfacesByInstance(ctx(), "inst_001")
	if err != nil {
		t.Fatalf("ListNetworkInterfacesByInstance: %v", err)
	}
	if len(nics) != 2 {
		t.Fatalf("expected 2 NICs, got %d", len(nics))
	}
	if nics[0].ID != "nic_001" {
		t.Errorf("first NIC ID = %q, want nic_001", nics[0].ID)
	}
	if nics[1].ID != "nic_002" {
		t.Errorf("second NIC ID = %q, want nic_002", nics[1].ID)
	}
}

func TestListNetworkInterfacesByInstance_ReturnsEmptySlice_WhenNoNICs(t *testing.T) {
	pool := &fakePool{queryRowsData: [][]any{}}
	r := newRepo(pool)

	nics, err := r.ListNetworkInterfacesByInstance(ctx(), "inst_001")
	if err != nil {
		t.Fatalf("ListNetworkInterfacesByInstance: %v", err)
	}
	if len(nics) != 0 {
		t.Errorf("expected empty slice, got %d items", len(nics))
	}
}
