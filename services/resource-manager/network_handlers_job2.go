package main

// network_handlers_job2.go — VM-P3A Job 2: Public connectivity maturity + Network policy maturity.
//
// This file EXTENDS network_handlers.go with:
//   A. Elastic IP (EIP) — allocate, get, list, associate, disassociate, release.
//   B. NAT Gateway — create, get, list, delete.
//   C. NIC Security Group Update — replace the SG set for a NIC.
//
// It uses the same helpers defined in network_handlers.go:
//   writeNetworkError, generateID, getNetworkRequestID, getNetworkPrincipalID.
//
// REPAIR (interface + helpers):
//   - PublicConnectivityRepo now embeds IGWNetworkRepo (not NetworkRepo) so that
//     mockPublicConnectivityRepo satisfies it without needing to re-implement IGW
//     methods that live in mockNetworkRepoP3A.
//   - Removed itoa() and containsSubstring() — snapshot_handlers.go already
//     declares itoa() in package main, causing a redeclaration compile error.
//     Use strconv.Itoa instead.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Allocation and Association",
//         "NAT Gateway Lifecycle", "Public Connectivity Contract".
//         vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model".

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Extended Repo Interface ───────────────────────────────────────────────────

// PublicConnectivityRepo covers EIP and NAT Gateway persistence.
// Embeds IGWNetworkRepo (which itself embeds NetworkRepo) so that a single
// *db.Repo instance in production satisfies all three interfaces.
// Test mocks embed mockNetworkRepoP3A (which satisfies IGWNetworkRepo) and add
// EIP + NAT GW stubs on top.
type PublicConnectivityRepo interface {
	IGWNetworkRepo

	// ElasticIP methods
	CreateElasticIP(ctx context.Context, row *db.ElasticIPRow) error
	GetElasticIPByID(ctx context.Context, id string) (*db.ElasticIPRow, error)
	ListElasticIPsByOwner(ctx context.Context, ownerPrincipalID string) ([]*db.ElasticIPRow, error)
	AssociateElasticIP(ctx context.Context, eipID, resourceID, associationType string) error
	DisassociateElasticIP(ctx context.Context, eipID string) error
	SoftDeleteElasticIP(ctx context.Context, id string) error
	GetElasticIPByAssociatedResource(ctx context.Context, resourceID string) (*db.ElasticIPRow, error)

	// NATGateway methods
	CreateNATGateway(ctx context.Context, row *db.NATGatewayRow) error
	GetNATGatewayByID(ctx context.Context, id string) (*db.NATGatewayRow, error)
	GetNATGatewayBySubnet(ctx context.Context, subnetID string) (*db.NATGatewayRow, error)
	ListNATGatewaysByVPC(ctx context.Context, vpcID string) ([]*db.NATGatewayRow, error)
	SoftDeleteNATGateway(ctx context.Context, id string) error

	// NIC security group update
	GetNetworkInterfaceByID(ctx context.Context, id string) (*db.NetworkInterfaceRow, error)
	ListSecurityGroupIDsByNIC(ctx context.Context, nicID string) ([]string, error)
	UpdateNICSecurityGroups(ctx context.Context, nicID string, sgIDs []string) error
	ValidateSecurityGroupsInVPC(ctx context.Context, sgIDs []string, vpcID, principalID string) error
}

// PublicConnectivityHandlers contains HTTP handlers for EIP, NAT Gateway, and NIC SG update.
type PublicConnectivityHandlers struct {
	repo PublicConnectivityRepo
}

// NewPublicConnectivityHandlers creates a PublicConnectivityHandlers instance.
func NewPublicConnectivityHandlers(repo PublicConnectivityRepo) *PublicConnectivityHandlers {
	return &PublicConnectivityHandlers{repo: repo}
}

// ── Request/Response types ────────────────────────────────────────────────────

// ElasticIPAllocateRequest is the request body for POST /v1/elastic_ips.
type ElasticIPAllocateRequest struct{}

// ElasticIPResponse is the API response shape for an Elastic IP resource.
type ElasticIPResponse struct {
	ID                   string    `json:"id"`
	Owner                string    `json:"owner"`
	PublicIP             string    `json:"public_ip"`
	AssociationType      string    `json:"association_type"`
	AssociatedResourceID *string   `json:"associated_resource_id,omitempty"`
	Status               string    `json:"status"`
	CreatedAt            time.Time `json:"created_at"`
}

// ElasticIPAssociateRequest is the request body for POST /v1/elastic_ips/{eip_id}/associate.
type ElasticIPAssociateRequest struct {
	NICID        *string `json:"nic_id,omitempty"`
	NATGatewayID *string `json:"nat_gateway_id,omitempty"`
}

// NATGatewayCreateRequest is the request body for POST /v1/vpcs/{vpc_id}/nat_gateways.
type NATGatewayCreateRequest struct {
	SubnetID    string `json:"subnet_id"`
	ElasticIPID string `json:"elastic_ip_id"`
}

// NATGatewayResponse is the API response shape for a NAT Gateway resource.
type NATGatewayResponse struct {
	ID          string    `json:"id"`
	Owner       string    `json:"owner"`
	VPCID       string    `json:"vpc_id"`
	SubnetID    string    `json:"subnet_id"`
	ElasticIPID string    `json:"elastic_ip_id"`
	PublicIP    string    `json:"public_ip"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// NICSecurityGroupsUpdateRequest is the request body for PUT /v1/nics/{nic_id}/security_groups.
type NICSecurityGroupsUpdateRequest struct {
	SecurityGroupIDs []string `json:"security_group_ids"`
}

// NICSecurityGroupsResponse is the response for NIC SG update.
type NICSecurityGroupsResponse struct {
	NICID            string   `json:"nic_id"`
	SecurityGroupIDs []string `json:"security_group_ids"`
}

// eipRowToResponse converts an ElasticIPRow to an ElasticIPResponse.
func eipRowToResponse(row *db.ElasticIPRow) ElasticIPResponse {
	return ElasticIPResponse{
		ID:                   row.ID,
		Owner:                row.OwnerPrincipalID,
		PublicIP:             row.PublicIP,
		AssociationType:      row.AssociationType,
		AssociatedResourceID: row.AssociatedResourceID,
		Status:               row.Status,
		CreatedAt:            row.CreatedAt,
	}
}

// ── Elastic IP Handlers ───────────────────────────────────────────────────────

// HandleAllocateElasticIP handles POST /v1/elastic_ips.
func (h *PublicConnectivityHandlers) HandleAllocateElasticIP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	var req ElasticIPAllocateRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
			return
		}
	}

	eipID := generateID("eip_")
	publicIP := simulatePublicIPAllocation(eipID)

	row := &db.ElasticIPRow{
		ID:               eipID,
		OwnerPrincipalID: principalID,
		PublicIP:         publicIP,
		AssociationType:  "none",
		Status:           "available",
	}

	if err := h.repo.CreateElasticIP(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to allocate elastic IP", "", requestID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(eipRowToResponse(row))
}

// HandleGetElasticIP handles GET /v1/elastic_ips/{eip_id}.
func (h *PublicConnectivityHandlers) HandleGetElasticIP(w http.ResponseWriter, r *http.Request, eipID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	row, err := h.repo.GetElasticIPByID(ctx, eipID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve elastic IP", "", requestID)
		return
	}
	if row == nil || row.DeletedAt != nil || row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "eip_not_found", "Elastic IP not found", "", requestID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(eipRowToResponse(row))
}

// HandleListElasticIPs handles GET /v1/elastic_ips.
func (h *PublicConnectivityHandlers) HandleListElasticIPs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	rows, err := h.repo.ListElasticIPsByOwner(ctx, principalID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to list elastic IPs", "", requestID)
		return
	}

	eips := make([]ElasticIPResponse, 0, len(rows))
	for _, row := range rows {
		eips = append(eips, eipRowToResponse(row))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"elastic_ips": eips})
}

// HandleAssociateElasticIP handles POST /v1/elastic_ips/{eip_id}/associate.
func (h *PublicConnectivityHandlers) HandleAssociateElasticIP(w http.ResponseWriter, r *http.Request, eipID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	eip, err := h.repo.GetElasticIPByID(ctx, eipID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve elastic IP", "", requestID)
		return
	}
	if eip == nil || eip.DeletedAt != nil || eip.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "eip_not_found", "Elastic IP not found", "", requestID)
		return
	}

	var req ElasticIPAssociateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.NICID == nil && req.NATGatewayID == nil {
		writeNetworkError(w, http.StatusBadRequest, "missing_field",
			"One of 'nic_id' or 'nat_gateway_id' is required", "", requestID)
		return
	}
	if req.NICID != nil && req.NATGatewayID != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value",
			"Only one of 'nic_id' or 'nat_gateway_id' may be specified", "", requestID)
		return
	}

	var resourceID, assocType string

	if req.NICID != nil {
		resourceID = *req.NICID
		assocType = "nic"
		nic, err := h.repo.GetNetworkInterfaceByID(ctx, resourceID)
		if err != nil {
			writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve NIC", "", requestID)
			return
		}
		if nic == nil || nic.DeletedAt != nil {
			writeNetworkError(w, http.StatusNotFound, "nic_not_found", "Network interface not found", "", requestID)
			return
		}
		vpc, err := h.repo.GetVPCByID(ctx, nic.VPCID)
		if err != nil {
			writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
			return
		}
		if vpc == nil || vpc.OwnerPrincipalID != principalID {
			writeNetworkError(w, http.StatusNotFound, "nic_not_found", "Network interface not found", "", requestID)
			return
		}
	} else {
		resourceID = *req.NATGatewayID
		assocType = "nat_gateway"
		natgw, err := h.repo.GetNATGatewayByID(ctx, resourceID)
		if err != nil {
			writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve NAT gateway", "", requestID)
			return
		}
		if natgw == nil || natgw.DeletedAt != nil || natgw.OwnerPrincipalID != principalID {
			writeNetworkError(w, http.StatusNotFound, "nat_gateway_not_found", "NAT gateway not found", "", requestID)
			return
		}
	}

	if err := h.repo.AssociateElasticIP(ctx, eipID, resourceID, assocType); err != nil {
		if _, ok := err.(*db.EIPAlreadyAssociatedError); ok {
			writeNetworkError(w, http.StatusConflict, "eip_already_associated",
				"Elastic IP is already associated to a resource; disassociate it first",
				"eip_id", requestID)
			return
		}
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to associate elastic IP", "", requestID)
		return
	}

	updated, err := h.repo.GetElasticIPByID(ctx, eipID)
	if err != nil || updated == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(eipRowToResponse(updated))
}

// HandleDisassociateElasticIP handles POST /v1/elastic_ips/{eip_id}/disassociate.
func (h *PublicConnectivityHandlers) HandleDisassociateElasticIP(w http.ResponseWriter, r *http.Request, eipID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	eip, err := h.repo.GetElasticIPByID(ctx, eipID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve elastic IP", "", requestID)
		return
	}
	if eip == nil || eip.DeletedAt != nil || eip.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "eip_not_found", "Elastic IP not found", "", requestID)
		return
	}

	if err := h.repo.DisassociateElasticIP(ctx, eipID); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to disassociate elastic IP", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleReleaseElasticIP handles DELETE /v1/elastic_ips/{eip_id}.
func (h *PublicConnectivityHandlers) HandleReleaseElasticIP(w http.ResponseWriter, r *http.Request, eipID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	eip, err := h.repo.GetElasticIPByID(ctx, eipID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve elastic IP", "", requestID)
		return
	}
	if eip == nil || eip.DeletedAt != nil || eip.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "eip_not_found", "Elastic IP not found", "", requestID)
		return
	}

	if eip.AssociationType != "none" {
		writeNetworkError(w, http.StatusConflict, "eip_still_associated",
			"Elastic IP must be disassociated before it can be released", "", requestID)
		return
	}

	if err := h.repo.SoftDeleteElasticIP(ctx, eipID); err != nil {
		if isEIPAssociatedErr(err) {
			writeNetworkError(w, http.StatusConflict, "eip_still_associated",
				"Elastic IP must be disassociated before it can be released", "", requestID)
			return
		}
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to release elastic IP", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── NAT Gateway Handlers ──────────────────────────────────────────────────────

// HandleCreateNATGateway handles POST /v1/vpcs/{vpc_id}/nat_gateways.
func (h *PublicConnectivityHandlers) HandleCreateNATGateway(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	vpc, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	var req NATGatewayCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.SubnetID == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'subnet_id' is required", "subnet_id", requestID)
		return
	}
	if req.ElasticIPID == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'elastic_ip_id' is required", "elastic_ip_id", requestID)
		return
	}

	subnet, err := h.repo.GetSubnetByID(ctx, req.SubnetID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve subnet", "", requestID)
		return
	}
	if subnet == nil || subnet.VPCID != vpcID || subnet.DeletedAt != nil {
		writeNetworkError(w, http.StatusNotFound, "subnet_not_found", "Subnet not found in VPC", "subnet_id", requestID)
		return
	}

	eip, err := h.repo.GetElasticIPByID(ctx, req.ElasticIPID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve elastic IP", "", requestID)
		return
	}
	if eip == nil || eip.DeletedAt != nil || eip.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "eip_not_found", "Elastic IP not found", "elastic_ip_id", requestID)
		return
	}
	if eip.AssociationType != "none" {
		writeNetworkError(w, http.StatusConflict, "eip_already_associated",
			"Elastic IP is already associated to a resource; disassociate it first",
			"elastic_ip_id", requestID)
		return
	}

	existing, err := h.repo.GetNATGatewayBySubnet(ctx, req.SubnetID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to check existing NAT gateway", "", requestID)
		return
	}
	if existing != nil {
		writeNetworkError(w, http.StatusConflict, "nat_gateway_already_exists",
			"Subnet already has a NAT gateway; delete it before creating a new one",
			"subnet_id", requestID)
		return
	}

	natgwID := generateID("natgw_")
	row := &db.NATGatewayRow{
		ID:               natgwID,
		OwnerPrincipalID: principalID,
		VPCID:            vpcID,
		SubnetID:         req.SubnetID,
		ElasticIPID:      req.ElasticIPID,
		Status:           "pending",
	}

	if err := h.repo.CreateNATGateway(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create NAT gateway", "", requestID)
		return
	}

	_ = h.repo.AssociateElasticIP(ctx, req.ElasticIPID, natgwID, "nat_gateway")

	resp := NATGatewayResponse{
		ID:          row.ID,
		Owner:       row.OwnerPrincipalID,
		VPCID:       row.VPCID,
		SubnetID:    row.SubnetID,
		ElasticIPID: row.ElasticIPID,
		PublicIP:    eip.PublicIP,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// HandleGetNATGateway handles GET /v1/vpcs/{vpc_id}/nat_gateways/{natgw_id}.
func (h *PublicConnectivityHandlers) HandleGetNATGateway(w http.ResponseWriter, r *http.Request, vpcID, natgwID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	vpc, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	row, err := h.repo.GetNATGatewayByID(ctx, natgwID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve NAT gateway", "", requestID)
		return
	}
	if row == nil || row.DeletedAt != nil || row.VPCID != vpcID || row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "nat_gateway_not_found", "NAT gateway not found", "", requestID)
		return
	}

	publicIP := ""
	if eip, err := h.repo.GetElasticIPByID(ctx, row.ElasticIPID); err == nil && eip != nil {
		publicIP = eip.PublicIP
	}

	resp := NATGatewayResponse{
		ID:          row.ID,
		Owner:       row.OwnerPrincipalID,
		VPCID:       row.VPCID,
		SubnetID:    row.SubnetID,
		ElasticIPID: row.ElasticIPID,
		PublicIP:    publicIP,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleListNATGateways handles GET /v1/vpcs/{vpc_id}/nat_gateways.
func (h *PublicConnectivityHandlers) HandleListNATGateways(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	vpc, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	rows, err := h.repo.ListNATGatewaysByVPC(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to list NAT gateways", "", requestID)
		return
	}

	natgws := make([]NATGatewayResponse, 0, len(rows))
	for _, row := range rows {
		publicIP := ""
		if eip, err := h.repo.GetElasticIPByID(ctx, row.ElasticIPID); err == nil && eip != nil {
			publicIP = eip.PublicIP
		}
		natgws = append(natgws, NATGatewayResponse{
			ID:          row.ID,
			Owner:       row.OwnerPrincipalID,
			VPCID:       row.VPCID,
			SubnetID:    row.SubnetID,
			ElasticIPID: row.ElasticIPID,
			PublicIP:    publicIP,
			Status:      row.Status,
			CreatedAt:   row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"nat_gateways": natgws})
}

// HandleDeleteNATGateway handles DELETE /v1/vpcs/{vpc_id}/nat_gateways/{natgw_id}.
func (h *PublicConnectivityHandlers) HandleDeleteNATGateway(w http.ResponseWriter, r *http.Request, vpcID, natgwID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	vpc, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	row, err := h.repo.GetNATGatewayByID(ctx, natgwID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve NAT gateway", "", requestID)
		return
	}
	if row == nil || row.DeletedAt != nil || row.VPCID != vpcID || row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "nat_gateway_not_found", "NAT gateway not found", "", requestID)
		return
	}

	if err := h.repo.SoftDeleteNATGateway(ctx, natgwID); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to delete NAT gateway", "", requestID)
		return
	}

	_ = h.repo.DisassociateElasticIP(ctx, row.ElasticIPID)

	w.WriteHeader(http.StatusNoContent)
}

// ── NIC Security Group Update Handler ────────────────────────────────────────

// HandleUpdateNICSecurityGroups handles PUT /v1/nics/{nic_id}/security_groups.
func (h *PublicConnectivityHandlers) HandleUpdateNICSecurityGroups(w http.ResponseWriter, r *http.Request, nicID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	var req NICSecurityGroupsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if len(req.SecurityGroupIDs) > 5 {
		writeNetworkError(w, http.StatusUnprocessableEntity, "sg_limit_exceeded",
			"A NIC can have at most 5 security groups (SG-I-3)",
			"security_group_ids", requestID)
		return
	}

	nic, err := h.repo.GetNetworkInterfaceByID(ctx, nicID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve NIC", "", requestID)
		return
	}
	if nic == nil || nic.DeletedAt != nil {
		writeNetworkError(w, http.StatusNotFound, "nic_not_found", "Network interface not found", "", requestID)
		return
	}

	vpc, err := h.repo.GetVPCByID(ctx, nic.VPCID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "nic_not_found", "Network interface not found", "", requestID)
		return
	}

	if len(req.SecurityGroupIDs) > 0 {
		if err := h.repo.ValidateSecurityGroupsInVPC(ctx, req.SecurityGroupIDs, nic.VPCID, principalID); err != nil {
			writeNetworkError(w, http.StatusBadRequest, "invalid_security_group",
				err.Error(), "security_group_ids", requestID)
			return
		}
	}

	if err := h.repo.UpdateNICSecurityGroups(ctx, nicID, req.SecurityGroupIDs); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to update security groups", "", requestID)
		return
	}

	sgIDs := req.SecurityGroupIDs
	if sgIDs == nil {
		sgIDs = []string{}
	}

	resp := NICSecurityGroupsResponse{
		NICID:            nicID,
		SecurityGroupIDs: sgIDs,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// simulatePublicIPAllocation returns a deterministic public IP for a given EIP ID.
// Uses strconv.Itoa instead of a local itoa() to avoid redeclaration with snapshot_handlers.go.
func simulatePublicIPAllocation(eipID string) string {
	if len(eipID) >= 6 {
		suffix := eipID[len(eipID)-2:]
		var octet int
		for _, c := range suffix {
			switch {
			case c >= '0' && c <= '9':
				octet = octet*16 + int(c-'0')
			case c >= 'a' && c <= 'f':
				octet = octet*16 + int(c-'a') + 10
			}
		}
		return "203.0.113." + strconv.Itoa(octet%254+1)
	}
	return "203.0.113.1"
}

// isEIPAssociatedErr reports whether err indicates an EIP-still-associated condition.
func isEIPAssociatedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "disassociate first")
}
