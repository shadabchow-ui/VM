package main

// network_handlers_test.go — Unit tests for VPC networking HTTP handlers.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.
// Phase 2 M9: VPC, Subnet, SecurityGroup, SecurityGroupRule handler tests.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Mock Repo ────────────────────────────────────────────────────────────────

type mockNetworkRepo struct {
	// VPC
	createVPCErr     error
	getVPCByIDRow    *db.VPCRow
	getVPCByIDErr    error
	listVPCsRows     []*db.VPCRow
	listVPCsErr      error
	softDeleteVPCErr error

	// Subnet
	createSubnetErr     error
	getSubnetByIDRow    *db.SubnetRow
	getSubnetByIDErr    error
	listSubnetsRows     []*db.SubnetRow
	listSubnetsErr      error
	softDeleteSubnetErr error

	// SecurityGroup
	createSGErr  error
	getSGByIDRow *db.SecurityGroupRow
	getSGByIDErr error
	listSGsRows  []*db.SecurityGroupRow
	listSGsErr   error

	// SecurityGroupRule
	createRuleErr error
	deleteRuleErr error
	listRulesRows []*db.SecurityGroupRuleRow
	listRulesErr  error

	// RouteTable
	createRouteTableErr     error
	getRouteTableByIDRow    *db.RouteTableRow
	getRouteTableByIDErr    error
	getDefaultRouteTableRow *db.RouteTableRow
	getDefaultRouteTableErr error
	listRouteTablesRows     []*db.RouteTableRow
	listRouteTablesErr      error
	softDeleteRouteTableErr error

	// RouteEntry
	createRouteEntryErr  error
	listRouteEntriesRows []*db.RouteEntryRow
	listRouteEntriesErr  error
	deleteRouteEntryErr  error
}

func (m *mockNetworkRepo) CreateVPC(_ context.Context, _ *db.VPCRow) error {
	return m.createVPCErr
}

func (m *mockNetworkRepo) GetVPCByID(_ context.Context, _ string) (*db.VPCRow, error) {
	return m.getVPCByIDRow, m.getVPCByIDErr
}

func (m *mockNetworkRepo) ListVPCsByOwner(_ context.Context, _ string) ([]*db.VPCRow, error) {
	return m.listVPCsRows, m.listVPCsErr
}

func (m *mockNetworkRepo) SoftDeleteVPC(_ context.Context, _ string) error {
	return m.softDeleteVPCErr
}

func (m *mockNetworkRepo) CreateSubnet(_ context.Context, _ *db.SubnetRow) error {
	return m.createSubnetErr
}

func (m *mockNetworkRepo) GetSubnetByID(_ context.Context, _ string) (*db.SubnetRow, error) {
	return m.getSubnetByIDRow, m.getSubnetByIDErr
}

func (m *mockNetworkRepo) ListSubnetsByVPC(_ context.Context, _ string) ([]*db.SubnetRow, error) {
	return m.listSubnetsRows, m.listSubnetsErr
}

func (m *mockNetworkRepo) SoftDeleteSubnet(_ context.Context, _ string) error {
	return m.softDeleteSubnetErr
}

func (m *mockNetworkRepo) CreateSecurityGroup(_ context.Context, _ *db.SecurityGroupRow) error {
	return m.createSGErr
}

func (m *mockNetworkRepo) GetSecurityGroupByID(_ context.Context, _ string) (*db.SecurityGroupRow, error) {
	return m.getSGByIDRow, m.getSGByIDErr
}

func (m *mockNetworkRepo) ListSecurityGroupsByVPC(_ context.Context, _ string) ([]*db.SecurityGroupRow, error) {
	return m.listSGsRows, m.listSGsErr
}

func (m *mockNetworkRepo) CreateSecurityGroupRule(_ context.Context, _ *db.SecurityGroupRuleRow) error {
	return m.createRuleErr
}

func (m *mockNetworkRepo) DeleteSecurityGroupRule(_ context.Context, _ string) error {
	return m.deleteRuleErr
}

func (m *mockNetworkRepo) ListSecurityGroupRulesBySecurityGroup(_ context.Context, _ string) ([]*db.SecurityGroupRuleRow, error) {
	return m.listRulesRows, m.listRulesErr
}

// RouteTable mock methods
func (m *mockNetworkRepo) CreateRouteTable(_ context.Context, _ *db.RouteTableRow) error {
	return m.createRouteTableErr
}

func (m *mockNetworkRepo) GetRouteTableByID(_ context.Context, _ string) (*db.RouteTableRow, error) {
	return m.getRouteTableByIDRow, m.getRouteTableByIDErr
}

func (m *mockNetworkRepo) GetDefaultRouteTableByVPC(_ context.Context, _ string) (*db.RouteTableRow, error) {
	return m.getDefaultRouteTableRow, m.getDefaultRouteTableErr
}

func (m *mockNetworkRepo) ListRouteTablesByVPC(_ context.Context, _ string) ([]*db.RouteTableRow, error) {
	return m.listRouteTablesRows, m.listRouteTablesErr
}

func (m *mockNetworkRepo) SoftDeleteRouteTable(_ context.Context, _ string) error {
	return m.softDeleteRouteTableErr
}

// RouteEntry mock methods
func (m *mockNetworkRepo) CreateRouteEntry(_ context.Context, _ *db.RouteEntryRow) error {
	return m.createRouteEntryErr
}

func (m *mockNetworkRepo) ListRouteEntriesByRouteTable(_ context.Context, _ string) ([]*db.RouteEntryRow, error) {
	return m.listRouteEntriesRows, m.listRouteEntriesErr
}

func (m *mockNetworkRepo) DeleteRouteEntry(_ context.Context, _ string) error {
	return m.deleteRouteEntryErr
}

// ── Test Helpers ─────────────────────────────────────────────────────────────

func testNetworkCtx(principalID string) context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, networkCtxKeyPrincipalID, principalID)
	ctx = context.WithValue(ctx, networkCtxKeyRequestID, "req_test123")
	return ctx
}

// testNetworkRequest creates a request with principal_id injected into context.
func testNetworkRequest(method, target string, body []byte, principalID string) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	ctx := testNetworkCtx(principalID)
	return req.WithContext(ctx)
}

// ── VPC Tests ────────────────────────────────────────────────────────────────

func TestNetworkHandlers_HandleCreateVPC_Success(t *testing.T) {
	repo := &mockNetworkRepo{}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"name": "my-vpc", "cidr": "10.0.0.0/16"}`)
	req := testNetworkRequest("POST", "/v1/vpcs", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateVPC(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp VPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Name != "my-vpc" {
		t.Errorf("name = %q, want my-vpc", resp.Name)
	}
	if resp.Status != "active" {
		t.Errorf("status = %q, want active", resp.Status)
	}
}

func TestNetworkHandlers_HandleCreateVPC_MissingName(t *testing.T) {
	repo := &mockNetworkRepo{}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"cidr": "10.0.0.0/16"}`)
	req := testNetworkRequest("POST", "/v1/vpcs", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateVPC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Code != "missing_field" {
		t.Errorf("error code = %q, want missing_field", errResp.Error.Code)
	}
}

func TestNetworkHandlers_HandleGetVPC_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetVPC(w, req, "vpc_001")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp VPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "vpc_001" {
		t.Errorf("id = %q, want vpc_001", resp.ID)
	}
}

func TestNetworkHandlers_HandleGetVPC_NotFound(t *testing.T) {
	repo := &mockNetworkRepo{getVPCByIDRow: nil}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_missing", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetVPC(w, req, "vpc_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleGetVPC_NotOwned_Returns404(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_other", // Different owner
			Name:             "other-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001", nil, "princ_001") // Requesting as different principal
	w := httptest.NewRecorder()

	h.HandleGetVPC(w, req, "vpc_001")

	// Should return 404 (not 403) to prevent enumeration
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (non-owned should return 404)", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleListVPCs_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		listVPCsRows: []*db.VPCRow{
			{ID: "vpc_001", OwnerPrincipalID: "princ_001", Name: "vpc-1", Status: "active", CreatedAt: now},
			{ID: "vpc_002", OwnerPrincipalID: "princ_001", Name: "vpc-2", Status: "active", CreatedAt: now},
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleListVPCs(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]VPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp["vpcs"]) != 2 {
		t.Errorf("vpc count = %d, want 2", len(resp["vpcs"]))
	}
}

// ── Subnet Tests ─────────────────────────────────────────────────────────────

func TestNetworkHandlers_HandleCreateSubnet_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"name": "my-subnet", "cidr": "10.0.1.0/24", "availability_zone": "us-east-1a"}`)
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/subnets", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateSubnet(w, req, "vpc_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp SubnetResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Name != "my-subnet" {
		t.Errorf("name = %q, want my-subnet", resp.Name)
	}
}

func TestNetworkHandlers_HandleCreateSubnet_VPCNotFound(t *testing.T) {
	repo := &mockNetworkRepo{getVPCByIDRow: nil}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"name": "my-subnet", "cidr": "10.0.1.0/24", "availability_zone": "us-east-1a"}`)
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_missing/subnets", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateSubnet(w, req, "vpc_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleListSubnets_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		listSubnetsRows: []*db.SubnetRow{
			{ID: "subnet_001", VPCID: "vpc_001", Name: "subnet-1", Status: "active", CreatedAt: now},
			{ID: "subnet_002", VPCID: "vpc_001", Name: "subnet-2", Status: "active", CreatedAt: now},
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/subnets", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleListSubnets(w, req, "vpc_001")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]SubnetResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp["subnets"]) != 2 {
		t.Errorf("subnet count = %d, want 2", len(resp["subnets"]))
	}
}

// ── Security Group Tests ─────────────────────────────────────────────────────

func TestNetworkHandlers_HandleCreateSecurityGroup_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"name": "my-sg", "vpc_id": "vpc_001", "description": "Test SG"}`)
	req := testNetworkRequest("POST", "/v1/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateSecurityGroup(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp SecurityGroupResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Name != "my-sg" {
		t.Errorf("name = %q, want my-sg", resp.Name)
	}
}

func TestNetworkHandlers_HandleCreateSecurityGroup_MissingVPCID(t *testing.T) {
	repo := &mockNetworkRepo{}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"name": "my-sg"}`)
	req := testNetworkRequest("POST", "/v1/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateSecurityGroup(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNetworkHandlers_HandleGetSecurityGroup_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
			CreatedAt:        now,
		},
		listRulesRows: []*db.SecurityGroupRuleRow{
			{ID: "sgr_001", SecurityGroupID: "sg_001", Direction: "ingress", Protocol: "tcp", CreatedAt: now},
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/security_groups/sg_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetSecurityGroup(w, req, "sg_001")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp SecurityGroupResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.IngressRules) != 1 {
		t.Errorf("ingress rules count = %d, want 1", len(resp.IngressRules))
	}
}

func TestNetworkHandlers_HandleListSecurityGroups_MissingVPCID(t *testing.T) {
	repo := &mockNetworkRepo{}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/security_groups", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleListSecurityGroups(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// ── Security Group Rule Tests ────────────────────────────────────────────────

func TestNetworkHandlers_HandleAddSecurityGroupRule_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"direction": "ingress", "protocol": "tcp", "port_from": 80, "port_to": 80, "cidr": "0.0.0.0/0"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp SecurityGroupRuleResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Direction != "ingress" {
		t.Errorf("direction = %q, want ingress", resp.Direction)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_InvalidDirection(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"direction": "invalid", "protocol": "tcp"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_BothCIDRAndSourceSG(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	// SG-I-5: Cannot specify both cidr and source_security_group_id
	body := []byte(`{"direction": "ingress", "protocol": "tcp", "cidr": "10.0.0.0/8", "source_security_group_id": "sg_002"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (SG-I-5 violation)", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestNetworkHandlers_HandleDeleteSecurityGroupRule_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/security_groups/sg_001/rules/sgr_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteSecurityGroupRule(w, req, "sg_001", "sgr_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestNetworkHandlers_HandleDeleteSecurityGroupRule_NotFound(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
			CreatedAt:        now,
		},
		deleteRuleErr: errors.New("rule not found"),
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/security_groups/sg_001/rules/sgr_missing", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteSecurityGroupRule(w, req, "sg_001", "sgr_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ── Route Table Tests ────────────────────────────────────────────────────────

func TestNetworkHandlers_HandleCreateRouteTable_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"name": "custom-rtb"}`)
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/route_tables", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateRouteTable(w, req, "vpc_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp RouteTableResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Name != "custom-rtb" {
		t.Errorf("name = %q, want custom-rtb", resp.Name)
	}
	if resp.IsDefault {
		t.Error("is_default should be false for user-created route tables")
	}
}

func TestNetworkHandlers_HandleCreateRouteTable_VPCNotFound(t *testing.T) {
	repo := &mockNetworkRepo{getVPCByIDRow: nil}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"name": "custom-rtb"}`)
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_missing/route_tables", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateRouteTable(w, req, "vpc_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleCreateRouteTable_MissingName(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{}`)
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/route_tables", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateRouteTable(w, req, "vpc_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNetworkHandlers_HandleGetRouteTable_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true,
			Status:    "active",
			CreatedAt: now,
		},
		listRouteEntriesRows: []*db.RouteEntryRow{
			{ID: "rte_001", RouteTableID: "rtb_001", DestinationCIDR: "10.0.0.0/16", TargetType: "local", Priority: 100, Status: "active", CreatedAt: now},
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/route_tables/rtb_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetRouteTable(w, req, "vpc_001", "rtb_001")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp RouteTableResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "rtb_001" {
		t.Errorf("id = %q, want rtb_001", resp.ID)
	}
	if len(resp.Routes) != 1 {
		t.Errorf("routes count = %d, want 1", len(resp.Routes))
	}
}

func TestNetworkHandlers_HandleGetRouteTable_NotFound(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: nil,
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/route_tables/rtb_missing", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetRouteTable(w, req, "vpc_001", "rtb_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleListRouteTables_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		listRouteTablesRows: []*db.RouteTableRow{
			{ID: "rtb_001", VPCID: "vpc_001", Name: "main", IsDefault: true, Status: "active", CreatedAt: now},
			{ID: "rtb_002", VPCID: "vpc_001", Name: "custom", IsDefault: false, Status: "active", CreatedAt: now},
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/route_tables", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleListRouteTables(w, req, "vpc_001")

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]RouteTableResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp["route_tables"]) != 2 {
		t.Errorf("route_tables count = %d, want 2", len(resp["route_tables"]))
	}
}

func TestNetworkHandlers_HandleDeleteRouteTable_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_002",
			VPCID:     "vpc_001",
			Name:      "custom",
			IsDefault: false, // Non-default can be deleted
			Status:    "active",
			CreatedAt: now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/route_tables/rtb_002", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteRouteTable(w, req, "vpc_001", "rtb_002")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestNetworkHandlers_HandleDeleteRouteTable_CannotDeleteDefault(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true, // Default cannot be deleted
			Status:    "active",
			CreatedAt: now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/route_tables/rtb_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteRouteTable(w, req, "vpc_001", "rtb_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (cannot delete default)", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestNetworkHandlers_HandleAddRouteEntry_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true,
			Status:    "active",
			CreatedAt: now,
		},
	}
	h := NewNetworkHandlers(repo)

	igwID := "igw_abc123"
	body := []byte(`{"destination_cidr": "0.0.0.0/0", "target_type": "igw", "target_id": "` + igwID + `"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var resp RouteEntryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DestinationCIDR != "0.0.0.0/0" {
		t.Errorf("destination_cidr = %q, want 0.0.0.0/0", resp.DestinationCIDR)
	}
	if resp.TargetType != "igw" {
		t.Errorf("target_type = %q, want igw", resp.TargetType)
	}
}

func TestNetworkHandlers_HandleAddRouteEntry_LocalNoTargetID(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true,
			Status:    "active",
			CreatedAt: now,
		},
	}
	h := NewNetworkHandlers(repo)

	// 'local' routes don't need target_id
	body := []byte(`{"destination_cidr": "10.0.0.0/16", "target_type": "local"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
}

func TestNetworkHandlers_HandleAddRouteEntry_NonLocalMissingTargetID(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true,
			Status:    "active",
			CreatedAt: now,
		},
	}
	h := NewNetworkHandlers(repo)

	// 'igw' routes require target_id
	body := []byte(`{"destination_cidr": "0.0.0.0/0", "target_type": "igw"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (missing target_id)", w.Code, http.StatusBadRequest)
	}
}

func TestNetworkHandlers_HandleAddRouteEntry_InvalidTargetType(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true,
			Status:    "active",
			CreatedAt: now,
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"destination_cidr": "0.0.0.0/0", "target_type": "vpn"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (invalid target_type)", w.Code, http.StatusBadRequest)
	}
}

func TestNetworkHandlers_HandleDeleteRouteEntry_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true,
			Status:    "active",
			CreatedAt: now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/route_tables/rtb_001/routes/rte_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteRouteEntry(w, req, "rtb_001", "rte_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestNetworkHandlers_HandleDeleteRouteEntry_NotFound(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			Status:           "active",
			CreatedAt:        now,
		},
		getRouteTableByIDRow: &db.RouteTableRow{
			ID:        "rtb_001",
			VPCID:     "vpc_001",
			Name:      "main",
			IsDefault: true,
			Status:    "active",
			CreatedAt: now,
		},
		deleteRouteEntryErr: errors.New("route not found"),
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/route_tables/rtb_001/routes/rte_missing", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteRouteEntry(w, req, "rtb_001", "rte_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ── VPC Delete Tests ─────────────────────────────────────────────────────────

func TestNetworkHandlers_HandleDeleteVPC_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteVPC(w, req, "vpc_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestNetworkHandlers_HandleDeleteVPC_NotFound(t *testing.T) {
	repo := &mockNetworkRepo{getVPCByIDRow: nil}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_missing", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteVPC(w, req, "vpc_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleDeleteVPC_NotOwned_Returns404(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_other", // Different owner
			Name:             "other-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteVPC(w, req, "vpc_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (non-owned returns 404)", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleDeleteVPC_RepoError(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
		softDeleteVPCErr: errors.New("db error"),
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteVPC(w, req, "vpc_001")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// ── Subnet Delete Tests ──────────────────────────────────────────────────────

func TestNetworkHandlers_HandleDeleteSubnet_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
		getSubnetByIDRow: &db.SubnetRow{
			ID:               "subnet_001",
			VPCID:            "vpc_001",
			Name:             "my-subnet",
			CIDRIPv4:         "10.0.1.0/24",
			AvailabilityZone: "us-east-1a",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/subnets/subnet_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteSubnet(w, req, "vpc_001", "subnet_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestNetworkHandlers_HandleDeleteSubnet_VPCNotFound(t *testing.T) {
	repo := &mockNetworkRepo{getVPCByIDRow: nil}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_missing/subnets/subnet_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteSubnet(w, req, "vpc_missing", "subnet_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleDeleteSubnet_SubnetNotFound(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
		getSubnetByIDRow: nil,
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/subnets/subnet_missing", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteSubnet(w, req, "vpc_001", "subnet_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleDeleteSubnet_WrongVPC(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
		getSubnetByIDRow: &db.SubnetRow{
			ID:               "subnet_001",
			VPCID:            "vpc_other", // Different VPC
			Name:             "other-subnet",
			CIDRIPv4:         "10.0.1.0/24",
			AvailabilityZone: "us-east-1a",
			Status:           "active",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/subnets/subnet_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteSubnet(w, req, "vpc_001", "subnet_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (subnet belongs to different VPC)", w.Code, http.StatusNotFound)
	}
}

func TestNetworkHandlers_HandleDeleteSubnet_RepoError(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepo{
		getVPCByIDRow: &db.VPCRow{
			ID:               "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-vpc",
			CIDRIPv4:         "10.0.0.0/16",
			Status:           "active",
			CreatedAt:        now,
		},
		getSubnetByIDRow: &db.SubnetRow{
			ID:               "subnet_001",
			VPCID:            "vpc_001",
			Name:             "my-subnet",
			CIDRIPv4:         "10.0.1.0/24",
			AvailabilityZone: "us-east-1a",
			Status:           "active",
			CreatedAt:        now,
		},
		softDeleteSubnetErr: errors.New("db error"),
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/subnets/subnet_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteSubnet(w, req, "vpc_001", "subnet_001")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// ── VM-P2A-S3: Port Range and Protocol-Port Compatibility Tests ───────────────
//
// These tests cover the validation rules added in VM-P2A-S3:
//   SG-I-4a: port_from/port_to must be in [0, 65535]
//   SG-I-4b: port_from must be <= port_to
//   SG-I-4c: icmp and all must not carry port fields
//   SG-I-2:  max 50 rules per security group

func testSGForRules(t *testing.T) (*mockNetworkRepo, *NetworkHandlers) {
	t.Helper()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
		},
	}
	return repo, NewNetworkHandlers(repo)
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_PortOutOfRange_Low(t *testing.T) {
	_, h := testSGForRules(t)

	body := []byte(`{"direction":"ingress","protocol":"tcp","port_from":-1,"port_to":80}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (SG-I-4a: port_from below 0)", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Target != "port_from" {
		t.Errorf("error target = %q, want port_from", errResp.Error.Target)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_PortOutOfRange_High(t *testing.T) {
	_, h := testSGForRules(t)

	body := []byte(`{"direction":"ingress","protocol":"tcp","port_from":80,"port_to":99999}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (SG-I-4a: port_to above 65535)", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Target != "port_to" {
		t.Errorf("error target = %q, want port_to", errResp.Error.Target)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_PortFromGtPortTo(t *testing.T) {
	_, h := testSGForRules(t)

	// port_from=443 > port_to=80 — invalid ordering
	body := []byte(`{"direction":"ingress","protocol":"tcp","port_from":443,"port_to":80}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (SG-I-4b: port_from > port_to)", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Target != "port_from" {
		t.Errorf("error target = %q, want port_from", errResp.Error.Target)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_ICMPWithPorts(t *testing.T) {
	_, h := testSGForRules(t)

	// SG-I-4c: icmp must not carry port fields
	body := []byte(`{"direction":"ingress","protocol":"icmp","port_from":0,"port_to":0}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (SG-I-4c: icmp does not use ports)", w.Code, http.StatusUnprocessableEntity)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Code != "invalid_rule" {
		t.Errorf("error code = %q, want invalid_rule", errResp.Error.Code)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_AllWithPorts(t *testing.T) {
	_, h := testSGForRules(t)

	// SG-I-4c: 'all' must not carry port fields
	body := []byte(`{"direction":"egress","protocol":"all","port_from":80,"port_to":80}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (SG-I-4c: 'all' does not use ports)", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_ValidPortRange(t *testing.T) {
	_, h := testSGForRules(t)

	// Valid TCP rule with legal port range
	cidr := "0.0.0.0/0"
	body := []byte(`{"direction":"ingress","protocol":"tcp","port_from":8080,"port_to":8090,"cidr":"0.0.0.0/0"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	_ = cidr
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (valid tcp port range 8080-8090 should be accepted)", w.Code, http.StatusCreated)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_ICMPNoPorts_Accepted(t *testing.T) {
	_, h := testSGForRules(t)

	// ICMP rule without port fields — must be accepted
	cidr := "0.0.0.0/0"
	_ = cidr
	body := []byte(`{"direction":"ingress","protocol":"icmp","cidr":"0.0.0.0/0"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (icmp rule without ports should be accepted)", w.Code, http.StatusCreated)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_RuleLimit(t *testing.T) {
	// Build 50 existing rules in the mock — next add should be rejected.
	existingRules := make([]*db.SecurityGroupRuleRow, 50)
	for i := range existingRules {
		existingRules[i] = &db.SecurityGroupRuleRow{
			ID:              "sgr_existing",
			SecurityGroupID: "sg_001",
			Direction:       "ingress",
			Protocol:        "tcp",
		}
	}
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
		},
		listRulesRows: existingRules,
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"direction":"ingress","protocol":"tcp","cidr":"10.0.0.0/8"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (SG-I-2: 50-rule limit)", w.Code, http.StatusUnprocessableEntity)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Code != "rule_limit_exceeded" {
		t.Errorf("error code = %q, want rule_limit_exceeded", errResp.Error.Code)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_UDPPortRange_BoundaryValues(t *testing.T) {
	_, h := testSGForRules(t)

	// UDP with port_from=0 and port_to=65535 — boundary values, must be accepted
	body := []byte(`{"direction":"egress","protocol":"udp","port_from":0,"port_to":65535}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d (udp 0-65535 must be accepted)", w.Code, http.StatusCreated)
	}
}

// ── VM Job 3: Cross-Tenant Security Group Safety Tests ────────────────────────
// These verify that security group ownership is enforced at all levels:
//   - Cannot add rules to an SG owned by a different principal (404).
//   - Cannot get/list SGs belonging to a different principal.
//   - Fail-close on unknown SG state — missing/absent SG is 404.

func TestNetworkHandlers_HandleAddSecurityGroupRule_CrossTenant_Returns404(t *testing.T) {
	// SG is owned by "princ_other", request comes from "princ_001"
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_other", // different owner
			Name:             "other-sg",
		},
	}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"direction":"ingress","protocol":"tcp","port_from":22,"port_to":22,"cidr":"0.0.0.0/0"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (cross-tenant rule add must not leak SG existence)", w.Code)
	}
}

func TestNetworkHandlers_HandleGetSecurityGroup_CrossTenant_Returns404(t *testing.T) {
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_other",
			Name:             "other-sg",
		},
	}
	h := NewNetworkHandlers(repo)

	req := testNetworkRequest("GET", "/v1/security_groups/sg_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetSecurityGroup(w, req, "sg_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (cross-tenant SG get must return 404)", w.Code)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_NilSG_Returns404(t *testing.T) {
	// SG does not exist at all.
	repo := &mockNetworkRepo{getSGByIDRow: nil}
	h := NewNetworkHandlers(repo)

	body := []byte(`{"direction":"ingress","protocol":"tcp","cidr":"0.0.0.0/0"}`)
	req := testNetworkRequest("POST", "/v1/security_groups/sg_missing/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (nil SG must return 404)", w.Code)
	}
}

func TestNetworkHandlers_HandleAddSecurityGroupRule_CIDRMustNotBeSourceSG_StillValidated(t *testing.T) {
	// SG-I-5: cannot specify both cidr AND source_security_group_id.
	// This test was already present but also verify the error response shape.
	now := time.Now()
	repo := &mockNetworkRepo{
		getSGByIDRow: &db.SecurityGroupRow{
			ID:               "sg_001",
			VPCID:            "vpc_001",
			OwnerPrincipalID: "princ_001",
			Name:             "my-sg",
			CreatedAt:        now,
		},
	}
	h := NewNetworkHandlers(repo)

	bothSG := "sg_002"
	body, _ := json.Marshal(SecurityGroupRuleCreateRequest{
		Direction:             "ingress",
		Protocol:              "tcp",
		CIDR:                  strPtr("10.0.0.0/8"),
		SourceSecurityGroupID: &bothSG,
	})
	req := testNetworkRequest("POST", "/v1/security_groups/sg_001/rules", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddSecurityGroupRule(w, req, "sg_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (SG-I-5: cannot set both CIDR and source_security_group_id)", w.Code)
	}

	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Error.Code != "invalid_rule" {
		t.Errorf("error code = %q, want invalid_rule", errResp.Error.Code)
	}
}

func strPtr(s string) *string { return &s }
