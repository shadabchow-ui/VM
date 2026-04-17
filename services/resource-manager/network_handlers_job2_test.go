package main

// network_handlers_job2_test.go — VM-P3A Job 2 handler tests.
//
// Covers EIP CRUD, NAT Gateway CRUD, and NIC SG update handlers.
// Uses the same test style as network_handlers_test.go (same package, same helpers).
//
// REPAIR:
//   - mockPublicConnectivityRepo now embeds mockNetworkRepoP3A (not mockNetworkRepo)
//     so it satisfies PublicConnectivityRepo, which embeds IGWNetworkRepo.
//     mockNetworkRepoP3A already provides all IGW and route-validation stubs.
//   - strPtr() removed — snapshot_handlers_test.go already defines it in package main.
//   - Struct initializers updated: mockNetworkRepo field → mockNetworkRepoP3A field
//     with inner mockNetworkRepo populated.
//
// Source: vm-14-03__blueprint__ §core_contracts, vm-14-02__blueprint__ §core_contracts,
//         11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Mock ─────────────────────────────────────────────────────────────────────

// mockPublicConnectivityRepo implements PublicConnectivityRepo for tests.
// Embeds mockNetworkRepoP3A (which satisfies IGWNetworkRepo) so that all
// IGW and route-validation methods are available via the embedded type.
// EIP, NAT GW, and NIC-SG fields are added directly on this struct.
type mockPublicConnectivityRepo struct {
	mockNetworkRepoP3A

	// ElasticIP
	createEIPErr             error
	getEIPByIDRow            *db.ElasticIPRow
	getEIPByIDErr            error
	listEIPsRows             []*db.ElasticIPRow
	listEIPsErr              error
	associateEIPErr          error
	disassociateEIPErr       error
	softDeleteEIPErr         error
	getEIPByAssocResourceRow *db.ElasticIPRow
	getEIPByAssocResourceErr error

	// NATGateway
	createNATGWErr      error
	getNATGWByIDRow     *db.NATGatewayRow
	getNATGWByIDErr     error
	getNATGWBySubnetRow *db.NATGatewayRow
	getNATGWBySubnetErr error
	listNATGWsRows      []*db.NATGatewayRow
	listNATGWsErr       error
	softDeleteNATGWErr  error

	// NIC
	getNICByIDRow  *db.NetworkInterfaceRow
	getNICByIDErr  error
	listSGIDsNIC   []string
	listSGIDsErr   error
	updateNICSGErr error
	validateSGsErr error
}

// ── ElasticIP mock methods ─────────────────────────────────────────────────────

func (m *mockPublicConnectivityRepo) CreateElasticIP(_ context.Context, _ *db.ElasticIPRow) error {
	return m.createEIPErr
}
func (m *mockPublicConnectivityRepo) GetElasticIPByID(_ context.Context, _ string) (*db.ElasticIPRow, error) {
	return m.getEIPByIDRow, m.getEIPByIDErr
}
func (m *mockPublicConnectivityRepo) ListElasticIPsByOwner(_ context.Context, _ string) ([]*db.ElasticIPRow, error) {
	return m.listEIPsRows, m.listEIPsErr
}
func (m *mockPublicConnectivityRepo) AssociateElasticIP(_ context.Context, _, _, _ string) error {
	return m.associateEIPErr
}
func (m *mockPublicConnectivityRepo) DisassociateElasticIP(_ context.Context, _ string) error {
	return m.disassociateEIPErr
}
func (m *mockPublicConnectivityRepo) SoftDeleteElasticIP(_ context.Context, _ string) error {
	return m.softDeleteEIPErr
}
func (m *mockPublicConnectivityRepo) GetElasticIPByAssociatedResource(_ context.Context, _ string) (*db.ElasticIPRow, error) {
	return m.getEIPByAssocResourceRow, m.getEIPByAssocResourceErr
}

// ── NATGateway mock methods ────────────────────────────────────────────────────

func (m *mockPublicConnectivityRepo) CreateNATGateway(_ context.Context, _ *db.NATGatewayRow) error {
	return m.createNATGWErr
}
func (m *mockPublicConnectivityRepo) GetNATGatewayByID(_ context.Context, _ string) (*db.NATGatewayRow, error) {
	return m.getNATGWByIDRow, m.getNATGWByIDErr
}
func (m *mockPublicConnectivityRepo) GetNATGatewayBySubnet(_ context.Context, _ string) (*db.NATGatewayRow, error) {
	return m.getNATGWBySubnetRow, m.getNATGWBySubnetErr
}
func (m *mockPublicConnectivityRepo) ListNATGatewaysByVPC(_ context.Context, _ string) ([]*db.NATGatewayRow, error) {
	return m.listNATGWsRows, m.listNATGWsErr
}
func (m *mockPublicConnectivityRepo) SoftDeleteNATGateway(_ context.Context, _ string) error {
	return m.softDeleteNATGWErr
}

// ── NIC mock methods ───────────────────────────────────────────────────────────

func (m *mockPublicConnectivityRepo) GetNetworkInterfaceByID(_ context.Context, _ string) (*db.NetworkInterfaceRow, error) {
	return m.getNICByIDRow, m.getNICByIDErr
}
func (m *mockPublicConnectivityRepo) ListSecurityGroupIDsByNIC(_ context.Context, _ string) ([]string, error) {
	return m.listSGIDsNIC, m.listSGIDsErr
}
func (m *mockPublicConnectivityRepo) UpdateNICSecurityGroups(_ context.Context, _ string, _ []string) error {
	return m.updateNICSGErr
}
func (m *mockPublicConnectivityRepo) ValidateSecurityGroupsInVPC(_ context.Context, _ []string, _, _ string) error {
	return m.validateSGsErr
}

// newPCHandlers creates a PublicConnectivityHandlers with the mock.
func newPCHandlers(m *mockPublicConnectivityRepo) *PublicConnectivityHandlers {
	return NewPublicConnectivityHandlers(m)
}

// ── EIP Tests ─────────────────────────────────────────────────────────────────

func TestJob2_HandleAllocateElasticIP_Success(t *testing.T) {
	repo := &mockPublicConnectivityRepo{}
	h := newPCHandlers(repo)

	req := testNetworkRequest("POST", "/v1/elastic_ips", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAllocateElasticIP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp ElasticIPResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AssociationType != "none" {
		t.Errorf("association_type = %q, want none", resp.AssociationType)
	}
	if resp.Status != "available" {
		t.Errorf("status = %q, want available", resp.Status)
	}
	if resp.PublicIP == "" {
		t.Error("public_ip must be non-empty")
	}
	if resp.Owner != "princ_001" {
		t.Errorf("owner = %q, want princ_001", resp.Owner)
	}
}

func TestJob2_HandleAllocateElasticIP_RepoError(t *testing.T) {
	repo := &mockPublicConnectivityRepo{createEIPErr: fmt.Errorf("db error")}
	h := newPCHandlers(repo)

	req := testNetworkRequest("POST", "/v1/elastic_ips", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAllocateElasticIP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestJob2_HandleGetElasticIP_Success(t *testing.T) {
	now := time.Now()
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:               "eip_001",
			OwnerPrincipalID: "princ_001",
			PublicIP:         "203.0.113.1",
			AssociationType:  "none",
			Status:           "available",
			CreatedAt:        now,
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("GET", "/v1/elastic_ips/eip_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetElasticIP(w, req, "eip_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp ElasticIPResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PublicIP != "203.0.113.1" {
		t.Errorf("public_ip = %q, want 203.0.113.1", resp.PublicIP)
	}
}

func TestJob2_HandleGetElasticIP_NotFound(t *testing.T) {
	repo := &mockPublicConnectivityRepo{getEIPByIDRow: nil}
	h := newPCHandlers(repo)

	req := testNetworkRequest("GET", "/v1/elastic_ips/eip_missing", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetElasticIP(w, req, "eip_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestJob2_HandleGetElasticIP_CrossOwner_Returns404(t *testing.T) {
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3 — 404 not 403 for cross-account access.
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:               "eip_001",
			OwnerPrincipalID: "princ_other",
			PublicIP:         "203.0.113.1",
			AssociationType:  "none",
			Status:           "available",
			CreatedAt:        time.Now(),
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("GET", "/v1/elastic_ips/eip_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetElasticIP(w, req, "eip_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-owner must return 404)", w.Code, http.StatusNotFound)
	}
}

func TestJob2_HandleListElasticIPs_Success(t *testing.T) {
	now := time.Now()
	repo := &mockPublicConnectivityRepo{
		listEIPsRows: []*db.ElasticIPRow{
			{ID: "eip_001", OwnerPrincipalID: "princ_001", PublicIP: "203.0.113.1", AssociationType: "none", Status: "available", CreatedAt: now},
			{ID: "eip_002", OwnerPrincipalID: "princ_001", PublicIP: "203.0.113.2", AssociationType: "nic", Status: "associated", CreatedAt: now},
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("GET", "/v1/elastic_ips", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleListElasticIPs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body struct {
		ElasticIPs []ElasticIPResponse `json:"elastic_ips"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.ElasticIPs) != 2 {
		t.Errorf("len(elastic_ips) = %d, want 2", len(body.ElasticIPs))
	}
}

func TestJob2_HandleAssociateElasticIP_NIC_Success(t *testing.T) {
	now := time.Now()
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:               "eip_001",
			OwnerPrincipalID: "princ_001",
			PublicIP:         "203.0.113.1",
			AssociationType:  "none",
			Status:           "available",
			CreatedAt:        now,
		},
		getNICByIDRow: &db.NetworkInterfaceRow{
			ID:         "nic_001",
			InstanceID: "inst_001",
			VPCID:      "vpc_001",
			SubnetID:   "subnet_001",
			PrivateIP:  "10.0.0.5",
			MACAddress: "02:ab:cd:ef:01:02",
			IsPrimary:  true,
			Status:     "attached",
			CreatedAt:  now,
		},
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{
					ID:               "vpc_001",
					OwnerPrincipalID: "princ_001",
					CIDRIPv4:         "10.0.0.0/16",
					Status:           "active",
					CreatedAt:        now,
				},
			},
		},
	}
	// After association, GetElasticIPByID returns the updated state.
	repo.getEIPByIDRow.AssociationType = "nic"
	repo.getEIPByIDRow.Status = "associated"
	assocID := "nic_001"
	repo.getEIPByIDRow.AssociatedResourceID = &assocID

	h := newPCHandlers(repo)

	body, _ := json.Marshal(ElasticIPAssociateRequest{NICID: strPtr("nic_001")})
	req := testNetworkRequest("POST", "/v1/elastic_ips/eip_001/associate", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAssociateElasticIP(w, req, "eip_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp ElasticIPResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AssociationType != "nic" {
		t.Errorf("association_type = %q, want nic", resp.AssociationType)
	}
}

func TestJob2_HandleAssociateElasticIP_BothTargets_Invalid(t *testing.T) {
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:               "eip_001",
			OwnerPrincipalID: "princ_001",
			PublicIP:         "203.0.113.1",
			AssociationType:  "none",
			Status:           "available",
			CreatedAt:        time.Now(),
		},
	}
	h := newPCHandlers(repo)

	body, _ := json.Marshal(ElasticIPAssociateRequest{
		NICID:        strPtr("nic_001"),
		NATGatewayID: strPtr("natgw_001"),
	})
	req := testNetworkRequest("POST", "/v1/elastic_ips/eip_001/associate", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAssociateElasticIP(w, req, "eip_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "invalid_value" {
		t.Errorf("code = %q, want invalid_value", errResp.Error.Code)
	}
}

func TestJob2_HandleAssociateElasticIP_NoTarget_Invalid(t *testing.T) {
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:               "eip_001",
			OwnerPrincipalID: "princ_001",
			AssociationType:  "none",
			Status:           "available",
			CreatedAt:        time.Now(),
		},
	}
	h := newPCHandlers(repo)

	body := []byte(`{}`)
	req := testNetworkRequest("POST", "/v1/elastic_ips/eip_001/associate", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAssociateElasticIP(w, req, "eip_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestJob2_HandleAssociateElasticIP_AlreadyAssociated_Conflict(t *testing.T) {
	existingResource := "nic_other"
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:                   "eip_001",
			OwnerPrincipalID:     "princ_001",
			AssociationType:      "nic",
			AssociatedResourceID: &existingResource,
			Status:               "associated",
			CreatedAt:            time.Now(),
		},
		getNICByIDRow: &db.NetworkInterfaceRow{
			ID: "nic_new", VPCID: "vpc_001", Status: "attached", CreatedAt: time.Now(),
		},
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{
					ID: "vpc_001", OwnerPrincipalID: "princ_001", Status: "active", CreatedAt: time.Now(),
				},
			},
		},
		associateEIPErr: &db.EIPAlreadyAssociatedError{EIPID: "eip_001", ExistingAssociation: "nic_other"},
	}
	h := newPCHandlers(repo)

	body, _ := json.Marshal(ElasticIPAssociateRequest{NICID: strPtr("nic_new")})
	req := testNetworkRequest("POST", "/v1/elastic_ips/eip_001/associate", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleAssociateElasticIP(w, req, "eip_001")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (already associated → 409)", w.Code, http.StatusConflict)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "eip_already_associated" {
		t.Errorf("code = %q, want eip_already_associated", errResp.Error.Code)
	}
}

func TestJob2_HandleDisassociateElasticIP_Success(t *testing.T) {
	assocID := "nic_001"
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:                   "eip_001",
			OwnerPrincipalID:     "princ_001",
			PublicIP:             "203.0.113.1",
			AssociationType:      "nic",
			AssociatedResourceID: &assocID,
			Status:               "associated",
			CreatedAt:            time.Now(),
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("POST", "/v1/elastic_ips/eip_001/disassociate", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDisassociateElasticIP(w, req, "eip_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestJob2_HandleReleaseElasticIP_Success(t *testing.T) {
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:               "eip_001",
			OwnerPrincipalID: "princ_001",
			PublicIP:         "203.0.113.1",
			AssociationType:  "none",
			Status:           "available",
			CreatedAt:        time.Now(),
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/elastic_ips/eip_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleReleaseElasticIP(w, req, "eip_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestJob2_HandleReleaseElasticIP_StillAssociated_Conflict(t *testing.T) {
	assocID := "nic_001"
	repo := &mockPublicConnectivityRepo{
		getEIPByIDRow: &db.ElasticIPRow{
			ID:                   "eip_001",
			OwnerPrincipalID:     "princ_001",
			AssociationType:      "nic",
			AssociatedResourceID: &assocID,
			Status:               "associated",
			CreatedAt:            time.Now(),
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/elastic_ips/eip_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleReleaseElasticIP(w, req, "eip_001")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (still associated → 409)", w.Code, http.StatusConflict)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "eip_still_associated" {
		t.Errorf("code = %q, want eip_still_associated", errResp.Error.Code)
	}
}

// ── NAT Gateway Tests ─────────────────────────────────────────────────────────

// testNATGWRepo builds a base repo for NAT gateway tests with a VPC, subnet, and free EIP.
func testNATGWRepo() *mockPublicConnectivityRepo {
	now := time.Now()
	return &mockPublicConnectivityRepo{
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{
					ID: "vpc_001", OwnerPrincipalID: "princ_001",
					CIDRIPv4: "10.0.0.0/16", Status: "active", CreatedAt: now,
				},
				getSubnetByIDRow: &db.SubnetRow{
					ID: "subnet_001", VPCID: "vpc_001",
					CIDRIPv4: "10.0.1.0/24", AvailabilityZone: "us-east-1a",
					Status: "active", CreatedAt: now,
				},
			},
		},
		getEIPByIDRow: &db.ElasticIPRow{
			ID:               "eip_001",
			OwnerPrincipalID: "princ_001",
			PublicIP:         "203.0.113.10",
			AssociationType:  "none",
			Status:           "available",
			CreatedAt:        now,
		},
		getNATGWBySubnetRow: nil,
	}
}

func TestJob2_HandleCreateNATGateway_Success(t *testing.T) {
	repo := testNATGWRepo()
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NATGatewayCreateRequest{
		SubnetID:    "subnet_001",
		ElasticIPID: "eip_001",
	})
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/nat_gateways", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateNATGateway(w, req, "vpc_001")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp NATGatewayResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SubnetID != "subnet_001" {
		t.Errorf("subnet_id = %q, want subnet_001", resp.SubnetID)
	}
	if resp.ElasticIPID != "eip_001" {
		t.Errorf("elastic_ip_id = %q, want eip_001", resp.ElasticIPID)
	}
	if resp.Status != "pending" {
		t.Errorf("status = %q, want pending", resp.Status)
	}
	if resp.PublicIP != "203.0.113.10" {
		t.Errorf("public_ip = %q, want 203.0.113.10", resp.PublicIP)
	}
}

func TestJob2_HandleCreateNATGateway_SubnetAlreadyHasNATGW_Conflict(t *testing.T) {
	repo := testNATGWRepo()
	repo.getNATGWBySubnetRow = &db.NATGatewayRow{
		ID: "natgw_existing", VPCID: "vpc_001", SubnetID: "subnet_001",
		Status: "available", CreatedAt: time.Now(),
	}
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NATGatewayCreateRequest{SubnetID: "subnet_001", ElasticIPID: "eip_001"})
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/nat_gateways", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateNATGateway(w, req, "vpc_001")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (one NAT GW per subnet → 409)", w.Code, http.StatusConflict)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "nat_gateway_already_exists" {
		t.Errorf("code = %q, want nat_gateway_already_exists", errResp.Error.Code)
	}
}

func TestJob2_HandleCreateNATGateway_EIPAlreadyAssociated_Conflict(t *testing.T) {
	repo := testNATGWRepo()
	assocID := "natgw_other"
	repo.getEIPByIDRow.AssociationType = "nat_gateway"
	repo.getEIPByIDRow.AssociatedResourceID = &assocID
	repo.getEIPByIDRow.Status = "associated"
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NATGatewayCreateRequest{SubnetID: "subnet_001", ElasticIPID: "eip_001"})
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/nat_gateways", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateNATGateway(w, req, "vpc_001")

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d (EIP already associated → 409)", w.Code, http.StatusConflict)
	}
}

func TestJob2_HandleCreateNATGateway_MissingSubnetID(t *testing.T) {
	repo := testNATGWRepo()
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NATGatewayCreateRequest{ElasticIPID: "eip_001"})
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_001/nat_gateways", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateNATGateway(w, req, "vpc_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Target != "subnet_id" {
		t.Errorf("target = %q, want subnet_id", errResp.Error.Target)
	}
}

func TestJob2_HandleCreateNATGateway_VPCNotFound(t *testing.T) {
	repo := &mockPublicConnectivityRepo{
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{getVPCByIDRow: nil},
		},
	}
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NATGatewayCreateRequest{SubnetID: "subnet_001", ElasticIPID: "eip_001"})
	req := testNetworkRequest("POST", "/v1/vpcs/vpc_missing/nat_gateways", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleCreateNATGateway(w, req, "vpc_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestJob2_HandleGetNATGateway_Success(t *testing.T) {
	now := time.Now()
	repo := &mockPublicConnectivityRepo{
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{
					ID: "vpc_001", OwnerPrincipalID: "princ_001", Status: "active", CreatedAt: now,
				},
			},
		},
		getNATGWByIDRow: &db.NATGatewayRow{
			ID:               "natgw_001",
			OwnerPrincipalID: "princ_001",
			VPCID:            "vpc_001",
			SubnetID:         "subnet_001",
			ElasticIPID:      "eip_001",
			Status:           "available",
			CreatedAt:        now,
		},
		getEIPByIDRow: &db.ElasticIPRow{
			ID: "eip_001", PublicIP: "203.0.113.10", OwnerPrincipalID: "princ_001",
			AssociationType: "nat_gateway", Status: "associated", CreatedAt: now,
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/nat_gateways/natgw_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetNATGateway(w, req, "vpc_001", "natgw_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp NATGatewayResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PublicIP != "203.0.113.10" {
		t.Errorf("public_ip = %q, want 203.0.113.10", resp.PublicIP)
	}
}

func TestJob2_HandleGetNATGateway_WrongVPC_Returns404(t *testing.T) {
	now := time.Now()
	repo := &mockPublicConnectivityRepo{
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{
					ID: "vpc_001", OwnerPrincipalID: "princ_001", Status: "active", CreatedAt: now,
				},
			},
		},
		getNATGWByIDRow: &db.NATGatewayRow{
			ID: "natgw_001", OwnerPrincipalID: "princ_001",
			VPCID:  "vpc_other",
			Status: "available", CreatedAt: now,
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/nat_gateways/natgw_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleGetNATGateway(w, req, "vpc_001", "natgw_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (wrong VPC → 404)", w.Code, http.StatusNotFound)
	}
}

func TestJob2_HandleListNATGateways_Success(t *testing.T) {
	now := time.Now()
	repo := &mockPublicConnectivityRepo{
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{ID: "vpc_001", OwnerPrincipalID: "princ_001", Status: "active", CreatedAt: now},
			},
		},
		listNATGWsRows: []*db.NATGatewayRow{
			{ID: "natgw_001", OwnerPrincipalID: "princ_001", VPCID: "vpc_001", SubnetID: "subnet_001", ElasticIPID: "eip_001", Status: "available", CreatedAt: now},
		},
		getEIPByIDRow: &db.ElasticIPRow{ID: "eip_001", PublicIP: "203.0.113.10", OwnerPrincipalID: "princ_001", AssociationType: "nat_gateway", Status: "associated", CreatedAt: now},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("GET", "/v1/vpcs/vpc_001/nat_gateways", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleListNATGateways(w, req, "vpc_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var body struct {
		NATGateways []NATGatewayResponse `json:"nat_gateways"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.NATGateways) != 1 {
		t.Errorf("len(nat_gateways) = %d, want 1", len(body.NATGateways))
	}
}

func TestJob2_HandleDeleteNATGateway_Success(t *testing.T) {
	now := time.Now()
	repo := &mockPublicConnectivityRepo{
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{ID: "vpc_001", OwnerPrincipalID: "princ_001", Status: "active", CreatedAt: now},
			},
		},
		getNATGWByIDRow: &db.NATGatewayRow{
			ID: "natgw_001", OwnerPrincipalID: "princ_001",
			VPCID: "vpc_001", SubnetID: "subnet_001", ElasticIPID: "eip_001",
			Status: "available", CreatedAt: now,
		},
	}
	h := newPCHandlers(repo)

	req := testNetworkRequest("DELETE", "/v1/vpcs/vpc_001/nat_gateways/natgw_001", nil, "princ_001")
	w := httptest.NewRecorder()

	h.HandleDeleteNATGateway(w, req, "vpc_001", "natgw_001")

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

// ── NIC SG Update Tests ───────────────────────────────────────────────────────

// testNICSGRepo builds a base repo for NIC SG update tests.
func testNICSGRepo() *mockPublicConnectivityRepo {
	now := time.Now()
	return &mockPublicConnectivityRepo{
		getNICByIDRow: &db.NetworkInterfaceRow{
			ID: "nic_001", InstanceID: "inst_001",
			VPCID: "vpc_001", SubnetID: "subnet_001",
			PrivateIP: "10.0.0.5", MACAddress: "02:ab:cd:ef:01:02",
			IsPrimary: true, Status: "attached", CreatedAt: now,
		},
		mockNetworkRepoP3A: mockNetworkRepoP3A{
			mockNetworkRepo: mockNetworkRepo{
				getVPCByIDRow: &db.VPCRow{
					ID: "vpc_001", OwnerPrincipalID: "princ_001",
					CIDRIPv4: "10.0.0.0/16", Status: "active", CreatedAt: now,
				},
			},
		},
	}
}

func TestJob2_HandleUpdateNICSecurityGroups_Success(t *testing.T) {
	repo := testNICSGRepo()
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NICSecurityGroupsUpdateRequest{
		SecurityGroupIDs: []string{"sg_001", "sg_002"},
	})
	req := testNetworkRequest("PUT", "/v1/nics/nic_001/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleUpdateNICSecurityGroups(w, req, "nic_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp NICSecurityGroupsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NICID != "nic_001" {
		t.Errorf("nic_id = %q, want nic_001", resp.NICID)
	}
	if len(resp.SecurityGroupIDs) != 2 {
		t.Errorf("len(security_group_ids) = %d, want 2", len(resp.SecurityGroupIDs))
	}
}

func TestJob2_HandleUpdateNICSecurityGroups_ClearAll_Success(t *testing.T) {
	repo := testNICSGRepo()
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NICSecurityGroupsUpdateRequest{SecurityGroupIDs: []string{}})
	req := testNetworkRequest("PUT", "/v1/nics/nic_001/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleUpdateNICSecurityGroups(w, req, "nic_001")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp NICSecurityGroupsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.SecurityGroupIDs) != 0 {
		t.Errorf("len(security_group_ids) = %d, want 0", len(resp.SecurityGroupIDs))
	}
}

func TestJob2_HandleUpdateNICSecurityGroups_TooMany_422(t *testing.T) {
	repo := testNICSGRepo()
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NICSecurityGroupsUpdateRequest{
		SecurityGroupIDs: []string{"sg_1", "sg_2", "sg_3", "sg_4", "sg_5", "sg_6"},
	})
	req := testNetworkRequest("PUT", "/v1/nics/nic_001/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleUpdateNICSecurityGroups(w, req, "nic_001")

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (SG-I-3 limit)", w.Code, http.StatusUnprocessableEntity)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "sg_limit_exceeded" {
		t.Errorf("code = %q, want sg_limit_exceeded", errResp.Error.Code)
	}
}

func TestJob2_HandleUpdateNICSecurityGroups_NICNotFound(t *testing.T) {
	repo := &mockPublicConnectivityRepo{getNICByIDRow: nil}
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NICSecurityGroupsUpdateRequest{SecurityGroupIDs: []string{}})
	req := testNetworkRequest("PUT", "/v1/nics/nic_missing/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleUpdateNICSecurityGroups(w, req, "nic_missing")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestJob2_HandleUpdateNICSecurityGroups_CrossOwnerVPC_Returns404(t *testing.T) {
	repo := testNICSGRepo()
	repo.mockNetworkRepoP3A.mockNetworkRepo.getVPCByIDRow.OwnerPrincipalID = "princ_other"
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NICSecurityGroupsUpdateRequest{SecurityGroupIDs: []string{"sg_001"}})
	req := testNetworkRequest("PUT", "/v1/nics/nic_001/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleUpdateNICSecurityGroups(w, req, "nic_001")

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-owner → 404)", w.Code, http.StatusNotFound)
	}
}

func TestJob2_HandleUpdateNICSecurityGroups_InvalidSG_BadRequest(t *testing.T) {
	repo := testNICSGRepo()
	repo.validateSGsErr = fmt.Errorf("security group sg_invalid is not in VPC vpc_001")
	h := newPCHandlers(repo)

	body, _ := json.Marshal(NICSecurityGroupsUpdateRequest{SecurityGroupIDs: []string{"sg_invalid"}})
	req := testNetworkRequest("PUT", "/v1/nics/nic_001/security_groups", body, "princ_001")
	w := httptest.NewRecorder()

	h.HandleUpdateNICSecurityGroups(w, req, "nic_001")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var errResp NetworkAPIError
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "invalid_security_group" {
		t.Errorf("code = %q, want invalid_security_group", errResp.Error.Code)
	}
}
