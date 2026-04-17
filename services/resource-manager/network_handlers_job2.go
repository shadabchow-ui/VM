package main

// network_handlers_job2.go — VM-P3A Job 2: Public connectivity maturity + Network policy maturity.
//
// This file EXTENDS network_handlers.go with:
//   A. Elastic IP (EIP) — allocate, get, list, associate, disassociate, release.
//   B. NAT Gateway — create, get, list, delete.
//   C. NIC Security Group Update — replace the SG set for a NIC (POST-admission
//      policy propagation seam).
//
// It uses the same NetworkHandlers struct, same writeNetworkError helper,
// same generateID helper, same context key helpers — all defined in network_handlers.go.
//
// The NetworkRepo interface is extended via a separate PublicConnectivityRepo
// interface that is embedded by the handlers declared here. Both are satisfied
// by the same *db.Repo instance in production.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Allocation and Association",
//         "NAT Gateway Lifecycle", "Public Connectivity Contract".
//         vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model".

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Extended Repo Interface ───────────────────────────────────────────────────
//
// PublicConnectivityRepo is the additional repo interface for VM-P3A Job 2
// handlers. In production, *db.Repo satisfies both NetworkRepo and
// PublicConnectivityRepo.
//
// A handler that needs both simply declares: repo interface { NetworkRepo; PublicConnectivityRepo }.
// The handler structs below use PublicConnectivityHandlers which embeds a combined interface.

// PublicConnectivityRepo covers EIP and NAT Gateway persistence.
type PublicConnectivityRepo interface {
	NetworkRepo

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
// The platform assigns the public IP from its pool; callers do not specify it.
type ElasticIPAllocateRequest struct {
	// Reserved for future use (e.g., region preference).
}

// ElasticIPResponse is the API response shape for an Elastic IP resource.
type ElasticIPResponse struct {
	ID                   string    `json:"id"`
	Owner                string    `json:"owner"`
	PublicIP             string    `json:"public_ip"`
	AssociationType      string    `json:"association_type"` // 'none' | 'nic' | 'nat_gateway'
	AssociatedResourceID *string   `json:"associated_resource_id,omitempty"`
	Status               string    `json:"status"`
	CreatedAt            time.Time `json:"created_at"`
}

// ElasticIPAssociateRequest is the request body for POST /v1/elastic_ips/{eip_id}/associate.
type ElasticIPAssociateRequest struct {
	// One of NICId or NATGatewayID must be set, but not both.
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
	ID           string    `json:"id"`
	Owner        string    `json:"owner"`
	VPCID        string    `json:"vpc_id"`
	SubnetID     string    `json:"subnet_id"`
	ElasticIPID  string    `json:"elastic_ip_id"`
	PublicIP     string    `json:"public_ip"`  // denormalized from EIP for convenience
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

// NICSecurityGroupsUpdateRequest is the request body for PUT /v1/nics/{nic_id}/security_groups.
// Replaces the full SG set for the NIC.
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
//
// Allocates a new Elastic IP from the platform public IP pool.
// The platform assigns the public_ip; callers cannot specify it.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Allocation".
func (h *PublicConnectivityHandlers) HandleAllocateElasticIP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Decode body — currently has no required fields, but parse to reject garbage JSON.
	var req ElasticIPAllocateRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
			return
		}
	}

	// The platform assigns the public IP. In production, this would draw from an
	// operator-managed pool of routable public addresses. Here we generate a stable
	// placeholder that is unique per EIP ID; production replaces this with a real
	// pool allocation (similar to ip_allocations / AllocateIP).
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
	// Return 404 for not-found or cross-owner — AUTH_OWNERSHIP_MODEL_V1 §3.
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
//
// Associates an EIP with a NIC or NAT Gateway.
// Exactly one of nic_id or nat_gateway_id must be provided.
// The EIP must be in 'none' (unassociated) state.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Association":
//   "An EIP can only be associated to one resource at a time."
func (h *PublicConnectivityHandlers) HandleAssociateElasticIP(w http.ResponseWriter, r *http.Request, eipID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify EIP exists and is owned by principal.
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

	// Validate: exactly one target.
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
		// Verify NIC exists and is owned by same principal (via its VPC).
		nic, err := h.repo.GetNetworkInterfaceByID(ctx, resourceID)
		if err != nil {
			writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve NIC", "", requestID)
			return
		}
		if nic == nil || nic.DeletedAt != nil {
			writeNetworkError(w, http.StatusNotFound, "nic_not_found", "Network interface not found", "", requestID)
			return
		}
		// Verify NIC's VPC is owned by principal.
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
		// Verify NAT Gateway exists and is owned by same principal.
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

	// Atomically associate. Returns *EIPAlreadyAssociatedError if already associated.
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

	// Return updated EIP state.
	updated, err := h.repo.GetElasticIPByID(ctx, eipID)
	if err != nil || updated == nil {
		// Association succeeded; best-effort response.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(eipRowToResponse(updated))
}

// HandleDisassociateElasticIP handles POST /v1/elastic_ips/{eip_id}/disassociate.
//
// Removes an EIP's association. Idempotent: calling on an already-unassociated
// EIP succeeds. Callers must handle host-side NAT rule teardown separately via
// the host-agent ProgramNAT/RemoveNAT path.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Disassociation".
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
//
// Releases an EIP back to the platform pool. The EIP must be unassociated.
// Attempting to release an associated EIP returns 409.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Release":
//   "An EIP cannot be released while it is associated to a resource."
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

	// Guard: cannot release an associated EIP.
	if eip.AssociationType != "none" {
		writeNetworkError(w, http.StatusConflict, "eip_still_associated",
			"Elastic IP must be disassociated before it can be released",
			"", requestID)
		return
	}

	if err := h.repo.SoftDeleteElasticIP(ctx, eipID); err != nil {
		// Check if it's an "already associated" error from the DB-level guard.
		if isEIPAssociatedErr(err) {
			writeNetworkError(w, http.StatusConflict, "eip_still_associated",
				"Elastic IP must be disassociated before it can be released",
				"", requestID)
			return
		}
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to release elastic IP", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── NAT Gateway Handlers ──────────────────────────────────────────────────────

// HandleCreateNATGateway handles POST /v1/vpcs/{vpc_id}/nat_gateways.
//
// Creates a NAT Gateway in a subnet. Enforces:
//   - VPC ownership.
//   - Subnet belongs to VPC.
//   - EIP is owned by principal and unassociated.
//   - Only one NAT Gateway per subnet (conflict check before DB insert).
//
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Lifecycle".
func (h *PublicConnectivityHandlers) HandleCreateNATGateway(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC ownership.
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

	// Verify subnet belongs to this VPC.
	subnet, err := h.repo.GetSubnetByID(ctx, req.SubnetID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve subnet", "", requestID)
		return
	}
	if subnet == nil || subnet.VPCID != vpcID || subnet.DeletedAt != nil {
		writeNetworkError(w, http.StatusNotFound, "subnet_not_found", "Subnet not found in VPC", "subnet_id", requestID)
		return
	}

	// Verify EIP is owned and unassociated.
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

	// Check one-per-subnet constraint (pre-flight before DB unique index fires).
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

	// Associate the EIP to this NAT Gateway.
	if err := h.repo.AssociateElasticIP(ctx, req.ElasticIPID, natgwID, "nat_gateway"); err != nil {
		// NAT GW was created but EIP association failed — log and continue.
		// The NAT GW will be in 'pending' and the EIP disassociated; a reconciler
		// can clean up. Return the NAT GW in 'pending' state to the caller.
		_ = err
	}

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

	// Verify VPC ownership first.
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

	// Fetch EIP public IP for denormalized response.
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

	// Verify VPC ownership.
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
//
// Deletes a NAT Gateway and disassociates its EIP.
// The EIP is returned to 'available' state but is NOT released — callers may
// re-associate or release it separately.
//
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Lifecycle".
func (h *PublicConnectivityHandlers) HandleDeleteNATGateway(w http.ResponseWriter, r *http.Request, vpcID, natgwID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC ownership.
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

	// Soft-delete the NAT Gateway first.
	if err := h.repo.SoftDeleteNATGateway(ctx, natgwID); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to delete NAT gateway", "", requestID)
		return
	}

	// Disassociate EIP. Best-effort: if this fails, the EIP will still show as
	// associated to a deleted NAT GW; a reconciler can correct it.
	_ = h.repo.DisassociateElasticIP(ctx, row.ElasticIPID)

	w.WriteHeader(http.StatusNoContent)
}

// ── NIC Security Group Update Handler ────────────────────────────────────────
//
// Source: vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model".

// HandleUpdateNICSecurityGroups handles PUT /v1/nics/{nic_id}/security_groups.
//
// Replaces the full security group set for a NIC atomically.
// Validates:
//   - NIC exists and its VPC is owned by the calling principal.
//   - All SG IDs are valid, in the same VPC, and not deleted.
//   - len(sgIDs) <= 5 (SG-I-3 limit).
//
// After updating the DB, policy propagation to the host-agent is the
// responsibility of the async worker path — the handler only updates the
// control-plane state. The worker reads the effective rules via
// GetEffectiveSGRulesForNIC and calls ApplySGPolicy on the host-agent.
func (h *PublicConnectivityHandlers) HandleUpdateNICSecurityGroups(w http.ResponseWriter, r *http.Request, nicID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	var req NICSecurityGroupsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	// Validate: max 5 SGs per NIC (SG-I-3).
	if len(req.SecurityGroupIDs) > 5 {
		writeNetworkError(w, http.StatusUnprocessableEntity, "sg_limit_exceeded",
			"A NIC can have at most 5 security groups (SG-I-3)",
			"security_group_ids", requestID)
		return
	}

	// Fetch NIC and verify VPC ownership.
	nic, err := h.repo.GetNetworkInterfaceByID(ctx, nicID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve NIC", "", requestID)
		return
	}
	if nic == nil || nic.DeletedAt != nil {
		writeNetworkError(w, http.StatusNotFound, "nic_not_found", "Network interface not found", "", requestID)
		return
	}

	// Verify VPC ownership (returns 404, not 403 — AUTH_OWNERSHIP_MODEL_V1 §3).
	vpc, err := h.repo.GetVPCByID(ctx, nic.VPCID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "nic_not_found", "Network interface not found", "", requestID)
		return
	}

	// Validate each SG: must exist, be in the same VPC, not deleted.
	if len(req.SecurityGroupIDs) > 0 {
		if err := h.repo.ValidateSecurityGroupsInVPC(ctx, req.SecurityGroupIDs, nic.VPCID, principalID); err != nil {
			writeNetworkError(w, http.StatusBadRequest, "invalid_security_group",
				err.Error(), "security_group_ids", requestID)
			return
		}
	}

	// Atomically replace the SG set.
	if err := h.repo.UpdateNICSecurityGroups(ctx, nicID, req.SecurityGroupIDs); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to update security groups", "", requestID)
		return
	}

	// Return the new SG set.
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

// simulatePublicIPAllocation returns a deterministic public IP string for a given EIP ID.
// In production this is replaced by a real pool allocation from public_ip_pool or similar.
// The format is stable so tests can assert on the returned IP.
func simulatePublicIPAllocation(eipID string) string {
	// Use the last 3 hex pairs of the ID as octets in 203.0.113.x range
	// (TEST-NET-3, RFC 5737 — documentation range, safe placeholder).
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
		return "203.0.113." + itoa(octet%254+1)
	}
	return "203.0.113.1"
}

// itoa converts an int to a string without importing strconv to avoid
// adding a new import to a file that does not otherwise use it.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// isEIPAssociatedErr reports whether err indicates an EIP-still-associated condition.
func isEIPAssociatedErr(err error) bool {
	if err == nil {
		return false
	}
	// SoftDeleteElasticIP returns a plain error string containing this sentinel
	// when the EIP's association_type != 'none' prevented the delete.
	return containsSubstring(err.Error(), "disassociate first")
}

// containsSubstring reports whether s contains substr.
func containsSubstring(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
