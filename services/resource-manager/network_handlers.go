package main

// network_handlers.go — Phase 2/P3A VPC networking HTTP handlers.
//
// Source: P2_VPC_NETWORK_CONTRACT §10 (API Endpoints Summary).
// Phase 2 M9: VPC, Subnet, SecurityGroup, SecurityGroupRule endpoints.
// VM-P3A Job 1: Extended VPC/Subnet with cidr_ipv6; extended RouteEntry with
//               address_family; added Internet Gateway CRUD handlers; enforced
//               IGW Exclusivity and NAT Anti-Loop contracts in HandleAddRouteEntry.
//
// REPAIR (interface split):
//   NetworkRepo   — original interface, unchanged. Existing tests that pass a
//                   plain *mockNetworkRepo satisfy this interface without change.
//   IGWNetworkRepo — new interface extending NetworkRepo with IGW CRUD and route
//                   validation helpers (VM-P3A Job 1 additions).
//   NetworkHandlers.repo     — NetworkRepo (used by VPC/Subnet/SG/Route handlers).
//   NetworkHandlers.igwRepo  — IGWNetworkRepo (used by IGW and route-validation handlers).
//                              nil when constructed via NewNetworkHandlers; non-nil
//                              when constructed via NewNetworkHandlersExtended.
//
// Source: vm-14-03__blueprint__ §core_contracts.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Request/Response Types ───────────────────────────────────────────────────

// VPCCreateRequest is the request body for POST /v1/vpcs.
// VM-P3A Job 1: Added CIDRIPv6 (optional).
type VPCCreateRequest struct {
	Name     string  `json:"name"`
	CIDR     string  `json:"cidr"`
	CIDRIPv6 *string `json:"cidr_ipv6,omitempty"`
}

// VPCResponse is the API response shape for a VPC resource.
// VM-P3A Job 1: Added CIDRIPv6.
type VPCResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	CIDR      string    `json:"cidr"`
	CIDRIPv6  *string   `json:"cidr_ipv6,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// SubnetCreateRequest is the request body for POST /v1/vpcs/{vpc_id}/subnets.
// VM-P3A Job 1: Added CIDRIPv6 (optional).
type SubnetCreateRequest struct {
	Name             string  `json:"name"`
	CIDR             string  `json:"cidr"`
	CIDRIPv6         *string `json:"cidr_ipv6,omitempty"`
	AvailabilityZone string  `json:"availability_zone"`
}

// SubnetResponse is the API response shape for a Subnet resource.
// VM-P3A Job 1: Added CIDRIPv6.
type SubnetResponse struct {
	ID               string    `json:"id"`
	VPCID            string    `json:"vpc_id"`
	Name             string    `json:"name"`
	Owner            string    `json:"owner"`
	CIDR             string    `json:"cidr"`
	CIDRIPv6         *string   `json:"cidr_ipv6,omitempty"`
	AvailabilityZone string    `json:"availability_zone"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
}

// SecurityGroupCreateRequest is the request body for POST /v1/security_groups.
type SecurityGroupCreateRequest struct {
	Name        string  `json:"name"`
	VPCID       string  `json:"vpc_id"`
	Description *string `json:"description,omitempty"`
}

// SecurityGroupResponse is the API response shape for a SecurityGroup resource.
type SecurityGroupResponse struct {
	ID           string                      `json:"id"`
	VPCID        string                      `json:"vpc_id"`
	Name         string                      `json:"name"`
	Owner        string                      `json:"owner"`
	Description  *string                     `json:"description,omitempty"`
	IngressRules []SecurityGroupRuleResponse `json:"ingress_rules"`
	EgressRules  []SecurityGroupRuleResponse `json:"egress_rules"`
	CreatedAt    time.Time                   `json:"created_at"`
}

// SecurityGroupRuleCreateRequest is the request body for adding a rule.
type SecurityGroupRuleCreateRequest struct {
	Direction             string  `json:"direction"`
	Protocol              string  `json:"protocol"`
	PortFrom              *int    `json:"port_from,omitempty"`
	PortTo                *int    `json:"port_to,omitempty"`
	CIDR                  *string `json:"cidr,omitempty"`
	SourceSecurityGroupID *string `json:"source_security_group_id,omitempty"`
}

// SecurityGroupRuleResponse is the API response shape for a rule.
type SecurityGroupRuleResponse struct {
	ID                    string  `json:"id"`
	Direction             string  `json:"direction"`
	Protocol              string  `json:"protocol"`
	PortFrom              *int    `json:"port_from,omitempty"`
	PortTo                *int    `json:"port_to,omitempty"`
	CIDR                  *string `json:"cidr,omitempty"`
	SourceSecurityGroupID *string `json:"source_security_group_id,omitempty"`
}

// RouteTableCreateRequest is the request body for POST /v1/vpcs/{vpc_id}/route_tables.
type RouteTableCreateRequest struct {
	Name string `json:"name"`
}

// RouteTableResponse is the API response shape for a RouteTable resource.
type RouteTableResponse struct {
	ID        string               `json:"id"`
	VPCID     string               `json:"vpc_id"`
	Name      string               `json:"name"`
	IsDefault bool                 `json:"is_default"`
	Status    string               `json:"status"`
	Routes    []RouteEntryResponse `json:"routes"`
	CreatedAt time.Time            `json:"created_at"`
}

// RouteEntryCreateRequest is the request body for POST /v1/route_tables/{rtb_id}/routes.
// VM-P3A Job 1: Added AddressFamily and NATGatewaySubnetID.
type RouteEntryCreateRequest struct {
	DestinationCIDR    string  `json:"destination_cidr"`
	TargetType         string  `json:"target_type"`
	TargetID           *string `json:"target_id,omitempty"`
	AddressFamily      string  `json:"address_family,omitempty"`
	NATGatewaySubnetID *string `json:"nat_gateway_subnet_id,omitempty"`
}

// RouteEntryResponse is the API response shape for a route entry.
// VM-P3A Job 1: Added AddressFamily.
type RouteEntryResponse struct {
	ID              string  `json:"id"`
	DestinationCIDR string  `json:"destination_cidr"`
	TargetType      string  `json:"target_type"`
	TargetID        *string `json:"target_id,omitempty"`
	AddressFamily   string  `json:"address_family"`
	Priority        int     `json:"priority"`
	Status          string  `json:"status"`
}

// InternetGatewayCreateRequest is the request body for POST /v1/vpcs/{vpc_id}/internet_gateways.
type InternetGatewayCreateRequest struct{}

// InternetGatewayResponse is the API response shape for an InternetGateway resource.
type InternetGatewayResponse struct {
	ID        string    `json:"id"`
	VPCID     string    `json:"vpc_id"`
	Owner     string    `json:"owner"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// NetworkAPIError is the standard error response shape per API_ERROR_CONTRACT_V1.
type NetworkAPIError struct {
	Error NetworkErrorDetail `json:"error"`
}

// NetworkErrorDetail contains the error details.
type NetworkErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Target    string `json:"target,omitempty"`
	RequestID string `json:"request_id"`
}

// ── Handler Struct ───────────────────────────────────────────────────────────

// NetworkHandlers contains HTTP handlers for VPC networking resources.
//
// repo: satisfies the original NetworkRepo interface — used by all base handlers.
// igwRepo: satisfies IGWNetworkRepo (extends NetworkRepo with IGW + validation).
//          Set to nil when constructed via NewNetworkHandlers (base tests).
//          Set to the same value as repo when constructed via NewNetworkHandlersExtended
//          (P3A tests / production wiring).
type NetworkHandlers struct {
	repo    NetworkRepo
	igwRepo IGWNetworkRepo
}

// NetworkRepo defines the repo interface required by the base network handlers.
// This interface is IDENTICAL to the current repo's version — do not add methods here.
// IGW and route-validation methods live in IGWNetworkRepo below.
type NetworkRepo interface {
	// VPC methods
	CreateVPC(ctx context.Context, row *db.VPCRow) error
	GetVPCByID(ctx context.Context, id string) (*db.VPCRow, error)
	ListVPCsByOwner(ctx context.Context, ownerPrincipalID string) ([]*db.VPCRow, error)
	SoftDeleteVPC(ctx context.Context, id string) error

	// Subnet methods
	CreateSubnet(ctx context.Context, row *db.SubnetRow) error
	GetSubnetByID(ctx context.Context, id string) (*db.SubnetRow, error)
	ListSubnetsByVPC(ctx context.Context, vpcID string) ([]*db.SubnetRow, error)
	SoftDeleteSubnet(ctx context.Context, id string) error

	// SecurityGroup methods
	CreateSecurityGroup(ctx context.Context, row *db.SecurityGroupRow) error
	GetSecurityGroupByID(ctx context.Context, id string) (*db.SecurityGroupRow, error)
	ListSecurityGroupsByVPC(ctx context.Context, vpcID string) ([]*db.SecurityGroupRow, error)

	// SecurityGroupRule methods
	CreateSecurityGroupRule(ctx context.Context, row *db.SecurityGroupRuleRow) error
	DeleteSecurityGroupRule(ctx context.Context, id string) error
	ListSecurityGroupRulesBySecurityGroup(ctx context.Context, sgID string) ([]*db.SecurityGroupRuleRow, error)

	// RouteTable methods
	CreateRouteTable(ctx context.Context, row *db.RouteTableRow) error
	GetRouteTableByID(ctx context.Context, id string) (*db.RouteTableRow, error)
	GetDefaultRouteTableByVPC(ctx context.Context, vpcID string) (*db.RouteTableRow, error)
	ListRouteTablesByVPC(ctx context.Context, vpcID string) ([]*db.RouteTableRow, error)
	SoftDeleteRouteTable(ctx context.Context, id string) error

	// RouteEntry methods
	CreateRouteEntry(ctx context.Context, row *db.RouteEntryRow) error
	ListRouteEntriesByRouteTable(ctx context.Context, routeTableID string) ([]*db.RouteEntryRow, error)
	DeleteRouteEntry(ctx context.Context, id string) error
}

// IGWNetworkRepo extends NetworkRepo with VM-P3A Job 1 additions:
// Internet Gateway CRUD and route-table validation helpers.
// Production *db.Repo satisfies both NetworkRepo and IGWNetworkRepo.
// Test mocks that need IGW/validation methods embed mockNetworkRepo and add
// their own IGW/validation stub methods.
type IGWNetworkRepo interface {
	NetworkRepo

	// Route validation helpers
	ValidateIGWExclusivity(ctx context.Context, routeTableID, igwID string) error
	ValidateRouteLoopFree(ctx context.Context, routeTableID, natGatewaySubnetID string) error

	// InternetGateway methods
	CreateInternetGateway(ctx context.Context, row *db.InternetGatewayRow) error
	GetInternetGatewayByID(ctx context.Context, id string) (*db.InternetGatewayRow, error)
	GetInternetGatewayByVPC(ctx context.Context, vpcID string) (*db.InternetGatewayRow, error)
	ListInternetGatewaysByOwner(ctx context.Context, ownerPrincipalID string) ([]*db.InternetGatewayRow, error)
	SoftDeleteInternetGateway(ctx context.Context, id string) error
}

// NewNetworkHandlers creates a NetworkHandlers with base NetworkRepo only.
// igwRepo is nil — IGW and route-validation handlers will panic if called.
// Use this constructor in tests that only exercise base VPC/Subnet/SG/Route handlers.
func NewNetworkHandlers(repo NetworkRepo) *NetworkHandlers {
	return &NetworkHandlers{repo: repo}
}

// NewNetworkHandlersExtended creates a NetworkHandlers with full IGWNetworkRepo.
// Use this constructor in P3A tests and production wiring.
func NewNetworkHandlersExtended(repo IGWNetworkRepo) *NetworkHandlers {
	return &NetworkHandlers{repo: repo, igwRepo: repo}
}

// ── ID Generation ───────────────────────────────────────────────────────────

func generateID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// ── IPv6 CIDR / route helpers ─────────────────────────────────────────────────

func isValidIPv6CIDR(cidr string) bool {
	if cidr == "" {
		return false
	}
	return strings.Contains(cidr, ":") && strings.Contains(cidr, "/")
}

func isDefaultRoute(cidr string) bool {
	return cidr == "0.0.0.0/0" || cidr == "::/0"
}

var validAddressFamilies = map[string]bool{
	"ipv4": true,
	"ipv6": true,
	"dual": true,
}

// ── VPC Handlers ─────────────────────────────────────────────────────────────

func (h *NetworkHandlers) HandleCreateVPC(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	var req VPCCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.Name == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'name' is required", "name", requestID)
		return
	}
	if len(req.Name) > 63 {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'name' must be 63 characters or less", "name", requestID)
		return
	}
	if req.CIDR == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'cidr' is required", "cidr", requestID)
		return
	}
	if req.CIDRIPv6 != nil && !isValidIPv6CIDR(*req.CIDRIPv6) {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'cidr_ipv6' must be a valid IPv6 CIDR (e.g. 2001:db8::/56)", "cidr_ipv6", requestID)
		return
	}

	vpcID := generateID("vpc_")
	now := time.Now().UTC()
	row := &db.VPCRow{
		ID:               vpcID,
		OwnerPrincipalID: principalID,
		Name:             req.Name,
		CIDRIPv4:         req.CIDR,
		CIDRIPv6:         req.CIDRIPv6,
		Status:           "active",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := h.repo.CreateVPC(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create VPC", "", requestID)
		return
	}

	resp := VPCResponse{
		ID:        row.ID,
		Name:      row.Name,
		Owner:     row.OwnerPrincipalID,
		CIDR:      row.CIDRIPv4,
		CIDRIPv6:  row.CIDRIPv6,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleGetVPC(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	row, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if row == nil {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}
	if row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	resp := VPCResponse{
		ID:        row.ID,
		Name:      row.Name,
		Owner:     row.OwnerPrincipalID,
		CIDR:      row.CIDRIPv4,
		CIDRIPv6:  row.CIDRIPv6,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleListVPCs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	rows, err := h.repo.ListVPCsByOwner(ctx, principalID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to list VPCs", "", requestID)
		return
	}

	vpcs := make([]VPCResponse, 0, len(rows))
	for _, row := range rows {
		vpcs = append(vpcs, VPCResponse{
			ID:        row.ID,
			Name:      row.Name,
			Owner:     row.OwnerPrincipalID,
			CIDR:      row.CIDRIPv4,
			CIDRIPv6:  row.CIDRIPv6,
			Status:    row.Status,
			CreatedAt: row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"vpcs": vpcs})
}

func (h *NetworkHandlers) HandleDeleteVPC(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	vpc, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}
	if vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	if err := h.repo.SoftDeleteVPC(ctx, vpcID); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to delete VPC", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Subnet Handlers ──────────────────────────────────────────────────────────

func (h *NetworkHandlers) HandleCreateSubnet(w http.ResponseWriter, r *http.Request, vpcID string) {
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

	var req SubnetCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.Name == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'name' is required", "name", requestID)
		return
	}
	if len(req.Name) > 63 {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'name' must be 63 characters or less", "name", requestID)
		return
	}
	if req.CIDR == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'cidr' is required", "cidr", requestID)
		return
	}
	if req.AvailabilityZone == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'availability_zone' is required", "availability_zone", requestID)
		return
	}
	if req.CIDRIPv6 != nil && !isValidIPv6CIDR(*req.CIDRIPv6) {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'cidr_ipv6' must be a valid IPv6 CIDR (e.g. 2001:db8:0:1::/64)", "cidr_ipv6", requestID)
		return
	}

	subnetID := generateID("subnet_")
	now := time.Now().UTC()
	row := &db.SubnetRow{
		ID:               subnetID,
		VPCID:            vpcID,
		Name:             req.Name,
		CIDRIPv4:         req.CIDR,
		CIDRIPv6:         req.CIDRIPv6,
		AvailabilityZone: req.AvailabilityZone,
		Status:           "active",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := h.repo.CreateSubnet(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create subnet", "", requestID)
		return
	}

	resp := SubnetResponse{
		ID:               row.ID,
		VPCID:            row.VPCID,
		Name:             row.Name,
		Owner:            principalID,
		CIDR:             row.CIDRIPv4,
		CIDRIPv6:         row.CIDRIPv6,
		AvailabilityZone: row.AvailabilityZone,
		Status:           row.Status,
		CreatedAt:        row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleGetSubnet(w http.ResponseWriter, r *http.Request, vpcID, subnetID string) {
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

	row, err := h.repo.GetSubnetByID(ctx, subnetID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve subnet", "", requestID)
		return
	}
	if row == nil || row.VPCID != vpcID {
		writeNetworkError(w, http.StatusNotFound, "subnet_not_found", "Subnet not found", "", requestID)
		return
	}

	resp := SubnetResponse{
		ID:               row.ID,
		VPCID:            row.VPCID,
		Name:             row.Name,
		Owner:            principalID,
		CIDR:             row.CIDRIPv4,
		CIDRIPv6:         row.CIDRIPv6,
		AvailabilityZone: row.AvailabilityZone,
		Status:           row.Status,
		CreatedAt:        row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleListSubnets(w http.ResponseWriter, r *http.Request, vpcID string) {
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

	rows, err := h.repo.ListSubnetsByVPC(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to list subnets", "", requestID)
		return
	}

	subnets := make([]SubnetResponse, 0, len(rows))
	for _, row := range rows {
		subnets = append(subnets, SubnetResponse{
			ID:               row.ID,
			VPCID:            row.VPCID,
			Name:             row.Name,
			Owner:            principalID,
			CIDR:             row.CIDRIPv4,
			CIDRIPv6:         row.CIDRIPv6,
			AvailabilityZone: row.AvailabilityZone,
			Status:           row.Status,
			CreatedAt:        row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"subnets": subnets})
}

func (h *NetworkHandlers) HandleDeleteSubnet(w http.ResponseWriter, r *http.Request, vpcID, subnetID string) {
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

	subnet, err := h.repo.GetSubnetByID(ctx, subnetID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve subnet", "", requestID)
		return
	}
	if subnet == nil || subnet.VPCID != vpcID {
		writeNetworkError(w, http.StatusNotFound, "subnet_not_found", "Subnet not found", "", requestID)
		return
	}

	if err := h.repo.SoftDeleteSubnet(ctx, subnetID); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to delete subnet", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Security Group Handlers ──────────────────────────────────────────────────

func (h *NetworkHandlers) HandleCreateSecurityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	var req SecurityGroupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.Name == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'name' is required", "name", requestID)
		return
	}
	if len(req.Name) > 63 {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'name' must be 63 characters or less", "name", requestID)
		return
	}
	if req.VPCID == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'vpc_id' is required", "vpc_id", requestID)
		return
	}

	vpc, err := h.repo.GetVPCByID(ctx, req.VPCID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	sgID := generateID("sg_")
	now := time.Now().UTC()
	row := &db.SecurityGroupRow{
		ID:               sgID,
		VPCID:            req.VPCID,
		OwnerPrincipalID: principalID,
		Name:             req.Name,
		Description:      req.Description,
		IsDefault:        false,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := h.repo.CreateSecurityGroup(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create security group", "", requestID)
		return
	}

	resp := SecurityGroupResponse{
		ID:           row.ID,
		VPCID:        row.VPCID,
		Name:         row.Name,
		Owner:        row.OwnerPrincipalID,
		Description:  row.Description,
		IngressRules: []SecurityGroupRuleResponse{},
		EgressRules:  []SecurityGroupRuleResponse{},
		CreatedAt:    row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleGetSecurityGroup(w http.ResponseWriter, r *http.Request, sgID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	row, err := h.repo.GetSecurityGroupByID(ctx, sgID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve security group", "", requestID)
		return
	}
	if row == nil || row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "security_group_not_found", "Security group not found", "", requestID)
		return
	}

	rules, err := h.repo.ListSecurityGroupRulesBySecurityGroup(ctx, sgID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve rules", "", requestID)
		return
	}

	ingressRules := []SecurityGroupRuleResponse{}
	egressRules := []SecurityGroupRuleResponse{}
	for _, rule := range rules {
		ruleResp := SecurityGroupRuleResponse{
			ID:                    rule.ID,
			Direction:             rule.Direction,
			Protocol:              rule.Protocol,
			PortFrom:              rule.PortFrom,
			PortTo:                rule.PortTo,
			CIDR:                  rule.CIDR,
			SourceSecurityGroupID: rule.SourceSecurityGroupID,
		}
		if rule.Direction == "ingress" {
			ingressRules = append(ingressRules, ruleResp)
		} else {
			egressRules = append(egressRules, ruleResp)
		}
	}

	resp := SecurityGroupResponse{
		ID:           row.ID,
		VPCID:        row.VPCID,
		Name:         row.Name,
		Owner:        row.OwnerPrincipalID,
		Description:  row.Description,
		IngressRules: ingressRules,
		EgressRules:  egressRules,
		CreatedAt:    row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleListSecurityGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)
	vpcID := r.URL.Query().Get("vpc_id")

	if vpcID == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Query parameter 'vpc_id' is required", "vpc_id", requestID)
		return
	}

	vpc, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	rows, err := h.repo.ListSecurityGroupsByVPC(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to list security groups", "", requestID)
		return
	}

	sgs := make([]SecurityGroupResponse, 0, len(rows))
	for _, row := range rows {
		sgs = append(sgs, SecurityGroupResponse{
			ID:           row.ID,
			VPCID:        row.VPCID,
			Name:         row.Name,
			Owner:        row.OwnerPrincipalID,
			Description:  row.Description,
			IngressRules: []SecurityGroupRuleResponse{},
			EgressRules:  []SecurityGroupRuleResponse{},
			CreatedAt:    row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"security_groups": sgs})
}

// ── Security Group Rule Handlers ─────────────────────────────────────────────

func (h *NetworkHandlers) HandleAddSecurityGroupRule(w http.ResponseWriter, r *http.Request, sgID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	sg, err := h.repo.GetSecurityGroupByID(ctx, sgID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve security group", "", requestID)
		return
	}
	if sg == nil || sg.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "security_group_not_found", "Security group not found", "", requestID)
		return
	}

	var req SecurityGroupRuleCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.Direction != "ingress" && req.Direction != "egress" {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'direction' must be 'ingress' or 'egress'", "direction", requestID)
		return
	}
	if req.Protocol != "tcp" && req.Protocol != "udp" && req.Protocol != "icmp" && req.Protocol != "all" {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'protocol' must be 'tcp', 'udp', 'icmp', or 'all'", "protocol", requestID)
		return
	}
	if req.CIDR != nil && req.SourceSecurityGroupID != nil {
		writeNetworkError(w, http.StatusUnprocessableEntity, "invalid_rule", "A rule cannot specify both 'cidr' and 'source_security_group_id'", "", requestID)
		return
	}

	if req.Protocol == "tcp" || req.Protocol == "udp" {
		if req.PortFrom != nil {
			if *req.PortFrom < 0 || *req.PortFrom > 65535 {
				writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'port_from' must be between 0 and 65535", "port_from", requestID)
				return
			}
		}
		if req.PortTo != nil {
			if *req.PortTo < 0 || *req.PortTo > 65535 {
				writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'port_to' must be between 0 and 65535", "port_to", requestID)
				return
			}
		}
		if req.PortFrom != nil && req.PortTo != nil && *req.PortFrom > *req.PortTo {
			writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'port_from' must be less than or equal to 'port_to'", "port_from", requestID)
			return
		}
	} else {
		if req.PortFrom != nil || req.PortTo != nil {
			writeNetworkError(w, http.StatusUnprocessableEntity, "invalid_rule",
				"Protocol '"+req.Protocol+"' does not use port numbers; omit 'port_from' and 'port_to'",
				"protocol", requestID)
			return
		}
	}

	existingRules, err := h.repo.ListSecurityGroupRulesBySecurityGroup(ctx, sgID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to check rule count", "", requestID)
		return
	}
	if len(existingRules) >= 50 {
		writeNetworkError(w, http.StatusUnprocessableEntity, "rule_limit_exceeded", "Security group cannot have more than 50 rules", "", requestID)
		return
	}

	ruleID := generateID("sgr_")
	now := time.Now().UTC()
	row := &db.SecurityGroupRuleRow{
		ID:                    ruleID,
		SecurityGroupID:       sgID,
		Direction:             req.Direction,
		Protocol:              req.Protocol,
		PortFrom:              req.PortFrom,
		PortTo:                req.PortTo,
		CIDR:                  req.CIDR,
		SourceSecurityGroupID: req.SourceSecurityGroupID,
		CreatedAt:             now,
	}

	if err := h.repo.CreateSecurityGroupRule(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create rule", "", requestID)
		return
	}

	resp := SecurityGroupRuleResponse{
		ID:                    row.ID,
		Direction:             row.Direction,
		Protocol:              row.Protocol,
		PortFrom:              row.PortFrom,
		PortTo:                row.PortTo,
		CIDR:                  row.CIDR,
		SourceSecurityGroupID: row.SourceSecurityGroupID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleDeleteSecurityGroupRule(w http.ResponseWriter, r *http.Request, sgID, ruleID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	sg, err := h.repo.GetSecurityGroupByID(ctx, sgID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve security group", "", requestID)
		return
	}
	if sg == nil || sg.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "security_group_not_found", "Security group not found", "", requestID)
		return
	}

	if err := h.repo.DeleteSecurityGroupRule(ctx, ruleID); err != nil {
		writeNetworkError(w, http.StatusNotFound, "rule_not_found", "Rule not found", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Route Table Handlers ─────────────────────────────────────────────────────

func (h *NetworkHandlers) HandleCreateRouteTable(w http.ResponseWriter, r *http.Request, vpcID string) {
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

	var req RouteTableCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.Name == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'name' is required", "name", requestID)
		return
	}
	if len(req.Name) > 63 {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'name' must be 63 characters or less", "name", requestID)
		return
	}

	rtbID := generateID("rtb_")
	now := time.Now().UTC()
	row := &db.RouteTableRow{
		ID:        rtbID,
		VPCID:     vpcID,
		Name:      req.Name,
		IsDefault: false,
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := h.repo.CreateRouteTable(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create route table", "", requestID)
		return
	}

	resp := RouteTableResponse{
		ID:        row.ID,
		VPCID:     row.VPCID,
		Name:      row.Name,
		IsDefault: row.IsDefault,
		Status:    row.Status,
		Routes:    []RouteEntryResponse{},
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleGetRouteTable(w http.ResponseWriter, r *http.Request, vpcID, rtbID string) {
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

	row, err := h.repo.GetRouteTableByID(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve route table", "", requestID)
		return
	}
	if row == nil || row.VPCID != vpcID {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	routes, err := h.repo.ListRouteEntriesByRouteTable(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve routes", "", requestID)
		return
	}

	routeResponses := make([]RouteEntryResponse, 0, len(routes))
	for _, route := range routes {
		routeResponses = append(routeResponses, RouteEntryResponse{
			ID:              route.ID,
			DestinationCIDR: route.DestinationCIDR,
			TargetType:      route.TargetType,
			TargetID:        route.TargetID,
			AddressFamily:   route.AddressFamily,
			Priority:        route.Priority,
			Status:          route.Status,
		})
	}

	resp := RouteTableResponse{
		ID:        row.ID,
		VPCID:     row.VPCID,
		Name:      row.Name,
		IsDefault: row.IsDefault,
		Status:    row.Status,
		Routes:    routeResponses,
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleListRouteTables(w http.ResponseWriter, r *http.Request, vpcID string) {
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

	rows, err := h.repo.ListRouteTablesByVPC(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to list route tables", "", requestID)
		return
	}

	rtbs := make([]RouteTableResponse, 0, len(rows))
	for _, row := range rows {
		rtbs = append(rtbs, RouteTableResponse{
			ID:        row.ID,
			VPCID:     row.VPCID,
			Name:      row.Name,
			IsDefault: row.IsDefault,
			Status:    row.Status,
			Routes:    []RouteEntryResponse{},
			CreatedAt: row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"route_tables": rtbs})
}

func (h *NetworkHandlers) HandleDeleteRouteTable(w http.ResponseWriter, r *http.Request, vpcID, rtbID string) {
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

	rtb, err := h.repo.GetRouteTableByID(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve route table", "", requestID)
		return
	}
	if rtb == nil || rtb.VPCID != vpcID {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	if rtb.IsDefault {
		writeNetworkError(w, http.StatusUnprocessableEntity, "cannot_delete_default", "Cannot delete the default route table", "", requestID)
		return
	}

	if err := h.repo.SoftDeleteRouteTable(ctx, rtbID); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to delete route table", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleAddRouteEntry handles POST /v1/route_tables/{rtb_id}/routes.
// VM-P3A Job 1: address_family, Gateway Default Route Target, IGW Exclusivity, NAT Anti-Loop.
func (h *NetworkHandlers) HandleAddRouteEntry(w http.ResponseWriter, r *http.Request, rtbID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	rtb, err := h.repo.GetRouteTableByID(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve route table", "", requestID)
		return
	}
	if rtb == nil {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	vpc, err := h.repo.GetVPCByID(ctx, rtb.VPCID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	var req RouteEntryCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	if req.DestinationCIDR == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'destination_cidr' is required", "destination_cidr", requestID)
		return
	}
	if req.TargetType == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'target_type' is required", "target_type", requestID)
		return
	}
	validTargetTypes := map[string]bool{"local": true, "igw": true, "nat": true, "peering": true}
	if !validTargetTypes[req.TargetType] {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'target_type' must be 'local', 'igw', 'nat', or 'peering'", "target_type", requestID)
		return
	}
	if req.TargetType != "local" && (req.TargetID == nil || *req.TargetID == "") {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'target_id' is required for non-local routes", "target_id", requestID)
		return
	}

	af := req.AddressFamily
	if af == "" {
		af = "ipv4"
	}
	if !validAddressFamilies[af] {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'address_family' must be 'ipv4', 'ipv6', or 'dual'", "address_family", requestID)
		return
	}

	// Gateway Default Route Target contract.
	if (req.TargetType == "igw" || req.TargetType == "nat") && !isDefaultRoute(req.DestinationCIDR) {
		writeNetworkError(w, http.StatusUnprocessableEntity, "invalid_route",
			"Target type '"+req.TargetType+"' is only valid for default routes (0.0.0.0/0 or ::/0)",
			"target_type", requestID)
		return
	}

	// IGW Exclusivity and NAT Anti-Loop require igwRepo (set by NewNetworkHandlersExtended).
	if h.igwRepo != nil {
		if req.TargetType == "igw" {
			if err := h.igwRepo.ValidateIGWExclusivity(ctx, rtbID, *req.TargetID); err != nil {
				if _, ok := err.(*db.IGWExclusivityError); ok {
					writeNetworkError(w, http.StatusUnprocessableEntity, "igw_not_attached",
						"Internet gateway is not attached to the VPC that owns this route table",
						"target_id", requestID)
					return
				}
				writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to validate internet gateway", "", requestID)
				return
			}
		}

		if req.TargetType == "nat" {
			if req.NATGatewaySubnetID == nil || *req.NATGatewaySubnetID == "" {
				writeNetworkError(w, http.StatusBadRequest, "missing_field",
					"Field 'nat_gateway_subnet_id' is required for routes targeting a NAT gateway",
					"nat_gateway_subnet_id", requestID)
				return
			}
			if err := h.igwRepo.ValidateRouteLoopFree(ctx, rtbID, *req.NATGatewaySubnetID); err != nil {
				if _, ok := err.(*db.NATLoopError); ok {
					writeNetworkError(w, http.StatusUnprocessableEntity, "routing_loop_detected",
						"Route would create a NAT routing loop: the NAT gateway is in a subnet that uses this route table",
						"nat_gateway_subnet_id", requestID)
					return
				}
				writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to validate route loop", "", requestID)
				return
			}
		}
	}

	rteID := generateID("rte_")
	now := time.Now().UTC()
	row := &db.RouteEntryRow{
		ID:              rteID,
		RouteTableID:    rtbID,
		DestinationCIDR: req.DestinationCIDR,
		TargetType:      req.TargetType,
		TargetID:        req.TargetID,
		AddressFamily:   af,
		Priority:        100,
		Status:          "active",
		CreatedAt:       now,
	}

	if err := h.repo.CreateRouteEntry(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create route", "", requestID)
		return
	}

	resp := RouteEntryResponse{
		ID:              row.ID,
		DestinationCIDR: row.DestinationCIDR,
		TargetType:      row.TargetType,
		TargetID:        row.TargetID,
		AddressFamily:   row.AddressFamily,
		Priority:        row.Priority,
		Status:          row.Status,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleDeleteRouteEntry(w http.ResponseWriter, r *http.Request, rtbID, rteID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	rtb, err := h.repo.GetRouteTableByID(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve route table", "", requestID)
		return
	}
	if rtb == nil {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	vpc, err := h.repo.GetVPCByID(ctx, rtb.VPCID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	if err := h.repo.DeleteRouteEntry(ctx, rteID); err != nil {
		writeNetworkError(w, http.StatusNotFound, "route_not_found", "Route not found", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Internet Gateway Handlers — VM-P3A Job 1 ─────────────────────────────────
// These handlers require igwRepo (set by NewNetworkHandlersExtended).

func (h *NetworkHandlers) HandleCreateInternetGateway(w http.ResponseWriter, r *http.Request, vpcID string) {
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

	existing, err := h.igwRepo.GetInternetGatewayByVPC(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to check existing internet gateway", "", requestID)
		return
	}
	if existing != nil {
		writeNetworkError(w, http.StatusConflict, "igw_already_attached",
			"VPC already has an internet gateway attached; detach or delete it before creating a new one",
			"", requestID)
		return
	}

	igwID := generateID("igw_")
	now := time.Now().UTC()
	row := &db.InternetGatewayRow{
		ID:               igwID,
		VPCID:            vpcID,
		OwnerPrincipalID: principalID,
		Status:           "available",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := h.igwRepo.CreateInternetGateway(ctx, row); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to create internet gateway", "", requestID)
		return
	}

	resp := InternetGatewayResponse{
		ID:        row.ID,
		VPCID:     row.VPCID,
		Owner:     row.OwnerPrincipalID,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleGetInternetGateway(w http.ResponseWriter, r *http.Request, vpcID, igwID string) {
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

	row, err := h.igwRepo.GetInternetGatewayByID(ctx, igwID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve internet gateway", "", requestID)
		return
	}
	if row == nil || row.VPCID != vpcID || row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "igw_not_found", "Internet gateway not found", "", requestID)
		return
	}

	resp := InternetGatewayResponse{
		ID:        row.ID,
		VPCID:     row.VPCID,
		Owner:     row.OwnerPrincipalID,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *NetworkHandlers) HandleListInternetGateways(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	rows, err := h.igwRepo.ListInternetGatewaysByOwner(ctx, principalID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to list internet gateways", "", requestID)
		return
	}

	igws := make([]InternetGatewayResponse, 0, len(rows))
	for _, row := range rows {
		igws = append(igws, InternetGatewayResponse{
			ID:        row.ID,
			VPCID:     row.VPCID,
			Owner:     row.OwnerPrincipalID,
			Status:    row.Status,
			CreatedAt: row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"internet_gateways": igws})
}

func (h *NetworkHandlers) HandleDeleteInternetGateway(w http.ResponseWriter, r *http.Request, vpcID, igwID string) {
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

	row, err := h.igwRepo.GetInternetGatewayByID(ctx, igwID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve internet gateway", "", requestID)
		return
	}
	if row == nil || row.VPCID != vpcID || row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "igw_not_found", "Internet gateway not found", "", requestID)
		return
	}

	if err := h.igwRepo.SoftDeleteInternetGateway(ctx, igwID); err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to delete internet gateway", "", requestID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func writeNetworkError(w http.ResponseWriter, status int, code, message, target, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(NetworkAPIError{
		Error: NetworkErrorDetail{
			Code:      code,
			Message:   message,
			Target:    target,
			RequestID: requestID,
		},
	})
}

func getNetworkRequestID(ctx context.Context) string {
	if v := ctx.Value(networkCtxKeyRequestID); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return generateID("req_")
}

func getNetworkPrincipalID(ctx context.Context) string {
	if v := ctx.Value(networkCtxKeyPrincipalID); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

type networkCtxKey string

const (
	networkCtxKeyPrincipalID networkCtxKey = "principal_id"
	networkCtxKeyRequestID   networkCtxKey = "request_id"
)

// ── URL Path Parsing Helpers ─────────────────────────────────────────────────

func extractPathParam(path string, prefix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimPrefix(rest, "/")
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

func extractTwoPathParams(path string, prefix string, middle string) (string, string) {
	if !strings.HasPrefix(path, prefix) {
		return "", ""
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimPrefix(rest, "/")

	idx := strings.Index(rest, "/")
	if idx < 0 {
		return rest, ""
	}
	first := rest[:idx]
	rest = rest[idx+1:]

	if !strings.HasPrefix(rest, middle) {
		return first, ""
	}
	rest = strings.TrimPrefix(rest, middle)
	rest = strings.TrimPrefix(rest, "/")

	if idx := strings.Index(rest, "/"); idx >= 0 {
		return first, rest[:idx]
	}
	return first, rest
}
