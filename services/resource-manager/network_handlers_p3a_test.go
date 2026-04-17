package main

// network_handlers_p3a_test.go — VM-P3A Job 1 handler tests.
//
// Covers the P3A additions to network_handlers.go:
//   - Dual-stack VPC creation (cidr_ipv6 optional field)
//   - Dual-stack Subnet creation (cidr_ipv6 optional field)
//   - IPv6 CIDR validation on VPC and Subnet create
//   - Internet Gateway CRUD (create, get, list, delete)
//   - IGW one-per-VPC conflict enforcement
//   - Route entry address_family field acceptance
//   - Gateway Default Route Target contract (igw/nat only valid for default routes)
//   - IGW Exclusivity contract (igw must be in same VPC as route table)
//   - NAT Anti-Loop contract (nat subnet must differ from associated subnets)
//
// Test style: mirrors network_handlers_test.go exactly.
// Mock style: extends mockNetworkRepo with IGW and route validation methods.
// Source: vm-14-01__blueprint__ §core_contracts, vm-14-03__blueprint__ §core_contracts.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Extended mock for P3A — adds IGW and route validation methods ─────────────

// mockNetworkRepoP3A extends mockNetworkRepo with VM-P3A Job 1 methods.
// It embeds the existing mock and adds IGW fields and route validation controls.
// This type satisfies IGWNetworkRepo (all NetworkRepo methods via embedding,
// plus IGW CRUD and route validation methods added below).
type mockNetworkRepoP3A struct {
	mockNetworkRepo

	// InternetGateway
	createIGWErr     error
	getIGWByIDRow    *db.InternetGatewayRow
	getIGWByIDErr    error
	getIGWByVPCRow   *db.InternetGatewayRow
	getIGWByVPCErr   error
	listIGWsRows     []*db.InternetGatewayRow
	listIGWsErr      error
	softDeleteIGWErr error

	// Route validation
	validateIGWErr  error
	validateLoopErr error
}

// InternetGateway mock methods
func (m *mockNetworkRepoP3A) CreateInternetGateway(_ context.Context, _ *db.InternetGatewayRow) error {
	return m.createIGWErr
}
func (m *mockNetworkRepoP3A) GetInternetGatewayByID(_ context.Context, _ string) (*db.InternetGatewayRow, error) {
	return m.getIGWByIDRow, m.getIGWByIDErr
}
func (m *mockNetworkRepoP3A) GetInternetGatewayByVPC(_ context.Context, _ string) (*db.InternetGatewayRow, error) {
	return m.getIGWByVPCRow, m.getIGWByVPCErr
}
func (m *mockNetworkRepoP3A) ListInternetGatewaysByOwner(_ context.Context, _ string) ([]*db.InternetGatewayRow, error) {
	return m.listIGWsRows, m.listIGWsErr
}
func (m *mockNetworkRepoP3A) SoftDeleteInternetGateway(_ context.Context, _ string) error {
	return m.softDeleteIGWErr
}

// Route validation mock methods
func (m *mockNetworkRepoP3A) ValidateIGWExclusivity(_ context.Context, _, _ string) error {
	return m.validateIGWErr
}
func (m *mockNetworkRepoP3A) ValidateRouteLoopFree(_ context.Context, _, _ string) error {
	return m.validateLoopErr
}

// newP3AHandlers creates a NetworkHandlers backed by a mockNetworkRepoP3A.
// Uses NewNetworkHandlersExtended so that igwRepo is populated — required for
// IGW handlers and route-validation logic in HandleAddRouteEntry.
func newP3AHandlers(m *mockNetworkRepoP3A) *NetworkHandlers {
	return NewNetworkHandlersExtended(m)
}

// ── VPC Dual-Stack Tests ──────────────────────────────────────────────────────

func TestP3A_HandleCreateVPC_DualStack_Success(t *testing.T) {
	repo := &mockNetworkRepoP3A{}
	h := newP3AHandlers(repo)

	ipv6 := "2001:db8::/56"
	body, _ := json.Marshal(VPCCreateRequest{
		Name:     "dual-vpc",
		CIDR:     "10.1.0.0/16",
		CIDRIPv6: &ipv6,
	})

	req := testNetworkRequest("POST", "/v1/vpcs", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateVPC(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp VPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CIDRIPv6 == nil {
		t.Error("cidr_ipv6 should be present in response for dual-stack VPC")
	} else if *resp.CIDRIPv6 != ipv6 {
		t.Errorf("cidr_ipv6 = %q, want %q", *resp.CIDRIPv6, ipv6)
	}
}

func TestP3A_HandleCreateVPC_IPv4Only_Success(t *testing.T) {
	// cidr_ipv6 omitted — must still succeed (backward-compatible).
	repo := &mockNetworkRepoP3A{}
	h := newP3AHandlers(repo)

	body := []byte(`{"name":"ipv4-vpc","cidr":"10.2.0.0/16"}`)
	req := testNetworkRequest("POST", "/v1/vpcs", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateVPC(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	var resp VPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CIDRIPv6 != nil {
		t.Errorf("cidr_ipv6 should be nil for IPv4-only VPC, got %q", *resp.CIDRIPv6)
	}
}

func TestP3A_HandleCreateVPC_InvalidIPv6CIDR(t *testing.T) {
	repo := &mockNetworkRepoP3A{}
	h := newP3AHandlers(repo)

	body := []byte(`{"name":"bad-vpc","cidr":"10.3.0.0/16","cidr_ipv6":"not-a-cidr"}`)
	req := testNetworkRequest("POST", "/v1/vpcs", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateVPC(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Target != "cidr_ipv6" {
		t.Errorf("error target = %q, want cidr_ipv6", errResp.Error.Target)
	}
	if errResp.Error.Code != "invalid_value" {
		t.Errorf("error code = %q, want invalid_value", errResp.Error.Code)
	}
}

// ── Subnet Dual-Stack Tests ───────────────────────────────────────────────────

func TestP3A_HandleCreateSubnet_DualStack_Success(t *testing.T) {
	ipv6 := "2001:db8:0:1::/64"
	repo := &mockNetworkRepoP3A{
		mockNetworkRepo: mockNetworkRepo{
			getVPCByIDRow: &db.VPCRow{
				ID:               "vpc_001",
				OwnerPrincipalID: "princ_001",
				Name:             "my-vpc",
				CIDRIPv4:         "10.0.0.0/16",
				Status:           "active",
				CreatedAt:        time.Now(),
			},
		},
	}
	h := newP3AHandlers(repo)

	body, _ := json.Marshal(SubnetCreateRequest{
		Name:             "dual-subnet",
		CIDR:             "10.0.1.0/24",
		CIDRIPv6:         &ipv6,
		AvailabilityZone: "us-east-1a",
	})

	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/subnets", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateSubnet(w, req, "vpc_001")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp SubnetResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CIDRIPv6 == nil || *resp.CIDRIPv6 != ipv6 {
		t.Errorf("cidr_ipv6 = %v, want %q", resp.CIDRIPv6, ipv6)
	}
}

func TestP3A_HandleCreateSubnet_InvalidIPv6CIDR(t *testing.T) {
	repo := &mockNetworkRepoP3A{
		mockNetworkRepo: mockNetworkRepo{
			getVPCByIDRow: &db.VPCRow{
				ID: "vpc_001", OwnerPrincipalID: "princ_001",
				Name: "v", CIDRIPv4: "10.0.0.0/16", Status: "active",
				CreatedAt: time.Now(),
			},
		},
	}
	h := newP3AHandlers(repo)

	body := []byte(`{"name":"bad-subnet","cidr":"10.0.2.0/24","cidr_ipv6":"10.0.0.0/24","availability_zone":"us-east-1a"}`)
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/subnets", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateSubnet(w, req, "vpc_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Target != "cidr_ipv6" {
		t.Errorf("error target = %q, want cidr_ipv6", errResp.Error.Target)
	}
}

// ── Internet Gateway Tests ────────────────────────────────────────────────────

// testVPCForIGW builds a mock with a VPC owned by princ_001 for IGW tests.
func testVPCForIGW() *mockNetworkRepoP3A {
	return &mockNetworkRepoP3A{
		mockNetworkRepo: mockNetworkRepo{
			getVPCByIDRow: &db.VPCRow{
				ID:               "vpc_001",
				OwnerPrincipalID: "princ_001",
				Name:             "my-vpc",
				CIDRIPv4:         "10.0.0.0/16",
				Status:           "active",
				CreatedAt:        time.Now(),
			},
		},
	}
}

func TestP3A_HandleCreateInternetGateway_Success(t *testing.T) {
	repo := testVPCForIGW()
	repo.getIGWByVPCRow = nil
	h := newP3AHandlers(repo)

	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/internet_gateways", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateInternetGateway(w, req, "vpc_001")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp InternetGatewayResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.VPCID != "vpc_001" {
		t.Errorf("vpc_id = %q, want vpc_001", resp.VPCID)
	}
	if resp.Status != "available" {
		t.Errorf("status = %q, want available", resp.Status)
	}
	if !strings.HasPrefix(resp.ID, "igw_") {
		t.Errorf("id %q should have prefix igw_", resp.ID)
	}
}

func TestP3A_HandleCreateInternetGateway_AlreadyAttached_Conflict(t *testing.T) {
	repo := testVPCForIGW()
	repo.getIGWByVPCRow = &db.InternetGatewayRow{
		ID:               "igw_existing",
		VPCID:            "vpc_001",
		OwnerPrincipalID: "princ_001",
		Status:           "available",
		CreatedAt:        time.Now(),
	}
	h := newP3AHandlers(repo)

	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/internet_gateways", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateInternetGateway(w, req, "vpc_001")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (IGW already attached should be 409)", w.Code, http.StatusConflict)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "igw_already_attached" {
		t.Errorf("error code = %q, want igw_already_attached", errResp.Error.Code)
	}
}

func TestP3A_HandleCreateInternetGateway_VPCNotFound(t *testing.T) {
	repo := &mockNetworkRepoP3A{
		mockNetworkRepo: mockNetworkRepo{getVPCByIDRow: nil},
	}
	h := newP3AHandlers(repo)

	req := testNetworkRequest("POST", "/v1/vpcs/vpc_missing/internet_gateways", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateInternetGateway(w, req, "vpc_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestP3A_HandleGetInternetGateway_Success(t *testing.T) {
	now := time.Now()
	repo := testVPCForIGW()
	repo.getIGWByIDRow = &db.InternetGatewayRow{
		ID:               "igw_001",
		VPCID:            "vpc_001",
		OwnerPrincipalID: "princ_001",
		Status:           "available",
		CreatedAt:        now,
	}
	h := newP3AHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/internet_gateways/igw_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetInternetGateway(w, req, "vpc_001", "igw_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp InternetGatewayResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "igw_001" {
		t.Errorf("id = %q, want igw_001", resp.ID)
	}
}

func TestP3A_HandleGetInternetGateway_WrongOwner_NotFound(t *testing.T) {
	repo := testVPCForIGW()
	repo.getIGWByIDRow = &db.InternetGatewayRow{
		ID:               "igw_001",
		VPCID:            "vpc_001",
		OwnerPrincipalID: "princ_other",
		Status:           "available",
		CreatedAt:        time.Now(),
	}
	h := newP3AHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/internet_gateways/igw_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetInternetGateway(w, req, "vpc_001", "igw_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-owner IGW must return 404)", w.Code, http.StatusNotFound)
	}
}

func TestP3A_HandleListInternetGateways_Success(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepoP3A{
		listIGWsRows: []*db.InternetGatewayRow{
			{ID: "igw_001", VPCID: "vpc_001", OwnerPrincipalID: "princ_001", Status: "available", CreatedAt: now},
			{ID: "igw_002", VPCID: "vpc_002", OwnerPrincipalID: "princ_001", Status: "available", CreatedAt: now},
		},
	}
	h := newP3AHandlers(repo)

	req := testNetworkRequest("GET", "/v1/internet_gateways", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleListInternetGateways(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body struct {
		InternetGateways []InternetGatewayResponse `json:"internet_gateways"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.InternetGateways) != 2 {
		t.Errorf("len(internet_gateways) = %d, want 2", len(body.InternetGateways))
	}
}

func TestP3A_HandleDeleteInternetGateway_Success(t *testing.T) {
	repo := testVPCForIGW()
	repo.getIGWByIDRow = &db.InternetGatewayRow{
		ID:               "igw_001",
		VPCID:            "vpc_001",
		OwnerPrincipalID: "princ_001",
		Status:           "available",
		CreatedAt:        time.Now(),
	}
	h := newP3AHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/internet_gateways/igw_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteInternetGateway(w, req, "vpc_001", "igw_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

// ── Route Entry Contract Tests ────────────────────────────────────────────────

// testRTBForRoutes creates a repo with a known route table owned by princ_001.
func testRTBForRoutes(validateIGWErr, validateLoopErr error) *mockNetworkRepoP3A {
	return &mockNetworkRepoP3A{
		mockNetworkRepo: mockNetworkRepo{
			getRouteTableByIDRow: &db.RouteTableRow{
				ID:        "rtb_001",
				VPCID:     "vpc_001",
				Name:      "my-rtb",
				IsDefault: false,
				Status:    "active",
				CreatedAt: time.Now(),
			},
			getVPCByIDRow: &db.VPCRow{
				ID:               "vpc_001",
				OwnerPrincipalID: "princ_001",
				CIDRIPv4:         "10.0.0.0/16",
				Status:           "active",
				CreatedAt:        time.Now(),
			},
		},
		validateIGWErr:  validateIGWErr,
		validateLoopErr: validateLoopErr,
	}
}

func TestP3A_HandleAddRouteEntry_IPv4_DefaultAddressFamily(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"10.100.0.0/16","target_type":"local"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp RouteEntryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AddressFamily != "ipv4" {
		t.Errorf("address_family = %q, want ipv4 (should default)", resp.AddressFamily)
	}
}

func TestP3A_HandleAddRouteEntry_IPv6_AddressFamily(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"2001:db8::/32","target_type":"local","address_family":"ipv6"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp RouteEntryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AddressFamily != "ipv6" {
		t.Errorf("address_family = %q, want ipv6", resp.AddressFamily)
	}
}

func TestP3A_HandleAddRouteEntry_InvalidAddressFamily(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"10.0.0.0/8","target_type":"local","address_family":"invalid"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Target != "address_family" {
		t.Errorf("error target = %q, want address_family", errResp.Error.Target)
	}
}

func TestP3A_HandleAddRouteEntry_GatewayDefaultRouteTarget_IGWNonDefault(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"10.0.0.0/8","target_type":"igw","target_id":"igw_001"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (igw on non-default route should be rejected)", w.Code, http.StatusUnprocessableEntity)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "invalid_route" {
		t.Errorf("error code = %q, want invalid_route", errResp.Error.Code)
	}
}

func TestP3A_HandleAddRouteEntry_GatewayDefaultRouteTarget_NATNonDefault(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"192.168.0.0/24","target_type":"nat","target_id":"nat_001","nat_gateway_subnet_id":"subnet_x"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestP3A_HandleAddRouteEntry_IGWExclusivity_Valid(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"0.0.0.0/0","target_type":"igw","target_id":"igw_001"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (IGW on same VPC should be accepted); body: %s",
			w.Code, http.StatusCreated, w.Body.String())
	}
}

func TestP3A_HandleAddRouteEntry_IGWExclusivity_Violated(t *testing.T) {
	exclusivityErr := &db.IGWExclusivityError{VPCID: "vpc_001", IGWID: "igw_other"}
	repo := testRTBForRoutes(exclusivityErr, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"0.0.0.0/0","target_type":"igw","target_id":"igw_other"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (IGW not attached to VPC → 422)", w.Code, http.StatusUnprocessableEntity)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "igw_not_attached" {
		t.Errorf("error code = %q, want igw_not_attached", errResp.Error.Code)
	}
	if errResp.Error.Target != "target_id" {
		t.Errorf("error target = %q, want target_id", errResp.Error.Target)
	}
}

func TestP3A_HandleAddRouteEntry_NATAntiLoop_MissingSubnetID(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"0.0.0.0/0","target_type":"nat","target_id":"nat_001"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (missing nat_gateway_subnet_id)", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Target != "nat_gateway_subnet_id" {
		t.Errorf("error target = %q, want nat_gateway_subnet_id", errResp.Error.Target)
	}
}

func TestP3A_HandleAddRouteEntry_NATAntiLoop_LoopDetected(t *testing.T) {
	loopErr := &db.NATLoopError{RouteTableID: "rtb_001", SubnetID: "subnet_same"}
	repo := testRTBForRoutes(nil, loopErr)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"0.0.0.0/0","target_type":"nat","target_id":"nat_001","nat_gateway_subnet_id":"subnet_same"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (NAT loop → 422)", w.Code, http.StatusUnprocessableEntity)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "routing_loop_detected" {
		t.Errorf("error code = %q, want routing_loop_detected", errResp.Error.Code)
	}
}

func TestP3A_HandleAddRouteEntry_NATAntiLoop_Valid(t *testing.T) {
	repo := testRTBForRoutes(nil, nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"0.0.0.0/0","target_type":"nat","target_id":"nat_001","nat_gateway_subnet_id":"subnet_other"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (valid NAT route should be accepted); body: %s",
			w.Code, http.StatusCreated, w.Body.String())
	}
}

func TestP3A_HandleAddRouteEntry_IGWExclusivity_InternalError(t *testing.T) {
	repo := testRTBForRoutes(errors.New("db unavailable"), nil)
	h := newP3AHandlers(repo)

	body := []byte(`{"destination_cidr":"0.0.0.0/0","target_type":"igw","target_id":"igw_001"}`)
	req := testNetworkRequest("POST", "/v1/route_tables/rtb_001/routes", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAddRouteEntry(w, req, "rtb_001")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (non-exclusivity error → 500)", w.Code, http.StatusInternalServerError)
	}
}

// ── Route Entry Response Shape Tests ─────────────────────────────────────────

func TestP3A_RouteEntryResponse_ContainsAddressFamily(t *testing.T) {
	now := time.Now()
	repo := &mockNetworkRepoP3A{
		mockNetworkRepo: mockNetworkRepo{
			getRouteTableByIDRow: &db.RouteTableRow{
				ID: "rtb_001", VPCID: "vpc_001", Name: "rtb", IsDefault: false,
				Status: "active", CreatedAt: now,
			},
			getVPCByIDRow: &db.VPCRow{
				ID: "vpc_001", OwnerPrincipalID: "princ_001",
				CIDRIPv4: "10.0.0.0/16", Status: "active", CreatedAt: now,
			},
			listRouteEntriesRows: []*db.RouteEntryRow{
				{
					ID: "rte_001", RouteTableID: "rtb_001",
					DestinationCIDR: "0.0.0.0/0", TargetType: "igw",
					AddressFamily: "ipv4", Priority: 100, Status: "active", CreatedAt: now,
				},
				{
					ID: "rte_002", RouteTableID: "rtb_001",
					DestinationCIDR: "::/0", TargetType: "igw",
					AddressFamily: "ipv6", Priority: 100, Status: "active", CreatedAt: now,
				},
			},
		},
	}
	h := newP3AHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/route_tables/rtb_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetRouteTable(w, req, "vpc_001", "rtb_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp RouteTableResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Routes) != 2 {
		t.Fatalf("routes len = %d, want 2", len(resp.Routes))
	}
	if resp.Routes[0].AddressFamily != "ipv4" {
		t.Errorf("routes[0].address_family = %q, want ipv4", resp.Routes[0].AddressFamily)
	}
	if resp.Routes[1].AddressFamily != "ipv6" {
		t.Errorf("routes[1].address_family = %q, want ipv6", resp.Routes[1].AddressFamily)
	}
}
