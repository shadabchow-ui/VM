package main

// network_handlers.go — Phase 2 VPC networking HTTP handlers.
//
// Source: P2_VPC_NETWORK_CONTRACT §10 (API Endpoints Summary).
// Phase 2 M9: VPC, Subnet, SecurityGroup, SecurityGroupRule endpoints.

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
type VPCCreateRequest struct {
	Name string `json:"name"`
	CIDR string `json:"cidr"`
}

// VPCResponse is the API response shape for a VPC resource.
type VPCResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	CIDR      string    `json:"cidr"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// SubnetCreateRequest is the request body for POST /v1/vpcs/{vpc_id}/subnets.
type SubnetCreateRequest struct {
	Name             string `json:"name"`
	CIDR             string `json:"cidr"`
	AvailabilityZone string `json:"availability_zone"`
}

// SubnetResponse is the API response shape for a Subnet resource.
type SubnetResponse struct {
	ID               string    `json:"id"`
	VPCID            string    `json:"vpc_id"`
	Name             string    `json:"name"`
	Owner            string    `json:"owner"`
	CIDR             string    `json:"cidr"`
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
	Direction             string  `json:"direction"` // "ingress" | "egress"
	Protocol              string  `json:"protocol"`  // "tcp" | "udp" | "icmp" | "all"
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
type RouteEntryCreateRequest struct {
	DestinationCIDR string  `json:"destination_cidr"`
	TargetType      string  `json:"target_type"` // "local", "igw", "nat", "peering"
	TargetID        *string `json:"target_id,omitempty"`
}

// RouteEntryResponse is the API response shape for a route entry.
type RouteEntryResponse struct {
	ID              string  `json:"id"`
	DestinationCIDR string  `json:"destination_cidr"`
	TargetType      string  `json:"target_type"`
	TargetID        *string `json:"target_id,omitempty"`
	Priority        int     `json:"priority"`
	Status          string  `json:"status"`
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
type NetworkHandlers struct {
	repo NetworkRepo
}

// NetworkRepo defines the repo interface required by network handlers.
type NetworkRepo interface {
	// VPC methods
	CreateVPC(ctx context.Context, row *db.VPCRow) error
	GetVPCByID(ctx context.Context, id string) (*db.VPCRow, error)
	ListVPCsByOwner(ctx context.Context, ownerPrincipalID string) ([]*db.VPCRow, error)

	// Subnet methods
	CreateSubnet(ctx context.Context, row *db.SubnetRow) error
	GetSubnetByID(ctx context.Context, id string) (*db.SubnetRow, error)
	ListSubnetsByVPC(ctx context.Context, vpcID string) ([]*db.SubnetRow, error)

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

// NewNetworkHandlers creates a new NetworkHandlers instance.
func NewNetworkHandlers(repo NetworkRepo) *NetworkHandlers {
	return &NetworkHandlers{repo: repo}
}

// ── ID Generation ───────────────────────────────────────────────────────────

// generateID creates a prefixed random ID (e.g., "vpc_a1b2c3d4e5f6").
func generateID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// ── VPC Handlers ─────────────────────────────────────────────────────────────

// HandleCreateVPC handles POST /v1/vpcs.
func (h *NetworkHandlers) HandleCreateVPC(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	var req VPCCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	// Validation
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

	vpcID := generateID("vpc_")
	now := time.Now().UTC()
	row := &db.VPCRow{
		ID:               vpcID,
		OwnerPrincipalID: principalID,
		Name:             req.Name,
		CIDRIPv4:         req.CIDR,
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
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// HandleGetVPC handles GET /v1/vpcs/{vpc_id}.
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

	// Ownership check: return 404 for non-owned resources (prevents enumeration)
	if row.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	resp := VPCResponse{
		ID:        row.ID,
		Name:      row.Name,
		Owner:     row.OwnerPrincipalID,
		CIDR:      row.CIDRIPv4,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleListVPCs handles GET /v1/vpcs.
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
			Status:    row.Status,
			CreatedAt: row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"vpcs": vpcs})
}

// ── Subnet Handlers ──────────────────────────────────────────────────────────

// HandleCreateSubnet handles POST /v1/vpcs/{vpc_id}/subnets.
func (h *NetworkHandlers) HandleCreateSubnet(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC exists and is owned by principal
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

	// Validation
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

	subnetID := generateID("subnet_")
	now := time.Now().UTC()
	row := &db.SubnetRow{
		ID:               subnetID,
		VPCID:            vpcID,
		Name:             req.Name,
		CIDRIPv4:         req.CIDR,
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
		AvailabilityZone: row.AvailabilityZone,
		Status:           row.Status,
		CreatedAt:        row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// HandleGetSubnet handles GET /v1/vpcs/{vpc_id}/subnets/{subnet_id}.
func (h *NetworkHandlers) HandleGetSubnet(w http.ResponseWriter, r *http.Request, vpcID, subnetID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC ownership first
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
		AvailabilityZone: row.AvailabilityZone,
		Status:           row.Status,
		CreatedAt:        row.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleListSubnets handles GET /v1/vpcs/{vpc_id}/subnets.
func (h *NetworkHandlers) HandleListSubnets(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC ownership
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
			AvailabilityZone: row.AvailabilityZone,
			Status:           row.Status,
			CreatedAt:        row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"subnets": subnets})
}

// ── Security Group Handlers ──────────────────────────────────────────────────

// HandleCreateSecurityGroup handles POST /v1/security_groups.
func (h *NetworkHandlers) HandleCreateSecurityGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	var req SecurityGroupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNetworkError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body", "", requestID)
		return
	}

	// Validation
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

	// Verify VPC exists and is owned by principal
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

// HandleGetSecurityGroup handles GET /v1/security_groups/{sg_id}.
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

	// Get rules
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

// HandleListSecurityGroups handles GET /v1/security_groups?vpc_id={vpc_id}.
func (h *NetworkHandlers) HandleListSecurityGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)
	vpcID := r.URL.Query().Get("vpc_id")

	if vpcID == "" {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Query parameter 'vpc_id' is required", "vpc_id", requestID)
		return
	}

	// Verify VPC ownership
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

// HandleAddSecurityGroupRule handles POST /v1/security_groups/{sg_id}/rules.
func (h *NetworkHandlers) HandleAddSecurityGroupRule(w http.ResponseWriter, r *http.Request, sgID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify security group exists and is owned by principal
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

	// Validation
	if req.Direction != "ingress" && req.Direction != "egress" {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'direction' must be 'ingress' or 'egress'", "direction", requestID)
		return
	}
	if req.Protocol != "tcp" && req.Protocol != "udp" && req.Protocol != "icmp" && req.Protocol != "all" {
		writeNetworkError(w, http.StatusBadRequest, "invalid_value", "Field 'protocol' must be 'tcp', 'udp', 'icmp', or 'all'", "protocol", requestID)
		return
	}
	// SG-I-5: A rule cannot reference both cidr and security_group_id
	if req.CIDR != nil && req.SourceSecurityGroupID != nil {
		writeNetworkError(w, http.StatusUnprocessableEntity, "invalid_rule", "A rule cannot specify both 'cidr' and 'source_security_group_id'", "", requestID)
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

// HandleDeleteSecurityGroupRule handles DELETE /v1/security_groups/{sg_id}/rules/{rule_id}.
func (h *NetworkHandlers) HandleDeleteSecurityGroupRule(w http.ResponseWriter, r *http.Request, sgID, ruleID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify security group ownership
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

// HandleCreateRouteTable handles POST /v1/vpcs/{vpc_id}/route_tables.
func (h *NetworkHandlers) HandleCreateRouteTable(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC exists and is owned by principal
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

	// Validation
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
		IsDefault: false, // User-created route tables are never default
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

// HandleGetRouteTable handles GET /v1/vpcs/{vpc_id}/route_tables/{rtb_id}.
func (h *NetworkHandlers) HandleGetRouteTable(w http.ResponseWriter, r *http.Request, vpcID, rtbID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC ownership first
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

	// Get routes for this route table
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

// HandleListRouteTables handles GET /v1/vpcs/{vpc_id}/route_tables.
func (h *NetworkHandlers) HandleListRouteTables(w http.ResponseWriter, r *http.Request, vpcID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC ownership
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
			Routes:    []RouteEntryResponse{}, // Don't include routes in list response
			CreatedAt: row.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"route_tables": rtbs})
}

// HandleDeleteRouteTable handles DELETE /v1/vpcs/{vpc_id}/route_tables/{rtb_id}.
func (h *NetworkHandlers) HandleDeleteRouteTable(w http.ResponseWriter, r *http.Request, vpcID, rtbID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Verify VPC ownership
	vpc, err := h.repo.GetVPCByID(ctx, vpcID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve VPC", "", requestID)
		return
	}
	if vpc == nil || vpc.OwnerPrincipalID != principalID {
		writeNetworkError(w, http.StatusNotFound, "vpc_not_found", "VPC not found", "", requestID)
		return
	}

	// Verify route table exists and belongs to VPC
	rtb, err := h.repo.GetRouteTableByID(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve route table", "", requestID)
		return
	}
	if rtb == nil || rtb.VPCID != vpcID {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	// Cannot delete default route table
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
func (h *NetworkHandlers) HandleAddRouteEntry(w http.ResponseWriter, r *http.Request, rtbID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Get route table and verify VPC ownership
	rtb, err := h.repo.GetRouteTableByID(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve route table", "", requestID)
		return
	}
	if rtb == nil {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	// Verify VPC ownership
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

	// Validation
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
	// 'local' routes don't need a target_id; others do
	if req.TargetType != "local" && (req.TargetID == nil || *req.TargetID == "") {
		writeNetworkError(w, http.StatusBadRequest, "missing_field", "Field 'target_id' is required for non-local routes", "target_id", requestID)
		return
	}

	rteID := generateID("rte_")
	now := time.Now().UTC()
	row := &db.RouteEntryRow{
		ID:              rteID,
		RouteTableID:    rtbID,
		DestinationCIDR: req.DestinationCIDR,
		TargetType:      req.TargetType,
		TargetID:        req.TargetID,
		Priority:        100, // Default priority
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
		Priority:        row.Priority,
		Status:          row.Status,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// HandleDeleteRouteEntry handles DELETE /v1/route_tables/{rtb_id}/routes/{rte_id}.
func (h *NetworkHandlers) HandleDeleteRouteEntry(w http.ResponseWriter, r *http.Request, rtbID, rteID string) {
	ctx := r.Context()
	requestID := getNetworkRequestID(ctx)
	principalID := getNetworkPrincipalID(ctx)

	// Get route table and verify VPC ownership
	rtb, err := h.repo.GetRouteTableByID(ctx, rtbID)
	if err != nil {
		writeNetworkError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve route table", "", requestID)
		return
	}
	if rtb == nil {
		writeNetworkError(w, http.StatusNotFound, "route_table_not_found", "Route table not found", "", requestID)
		return
	}

	// Verify VPC ownership
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

// getNetworkRequestID extracts the request ID from context.
// In production, this would be set by middleware.
func getNetworkRequestID(ctx context.Context) string {
	if v := ctx.Value(networkCtxKeyRequestID); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return generateID("req_")
}

// getNetworkPrincipalID extracts the authenticated principal ID from context.
// In production, this would be set by auth middleware.
func getNetworkPrincipalID(ctx context.Context) string {
	if v := ctx.Value(networkCtxKeyPrincipalID); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Context keys for network handlers
type networkCtxKey string

const (
	networkCtxKeyPrincipalID networkCtxKey = "principal_id"
	networkCtxKeyRequestID   networkCtxKey = "request_id"
)

// ── URL Path Parsing Helpers ─────────────────────────────────────────────────

// extractPathParam extracts a path segment from a URL path pattern.
// For pattern "/v1/vpcs/{vpc_id}" and path "/v1/vpcs/vpc_123", returns "vpc_123".
func extractPathParam(path string, prefix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	// Remove leading slash if present
	rest = strings.TrimPrefix(rest, "/")
	// Return the first path segment
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// extractTwoPathParams extracts two consecutive path parameters.
// For pattern "/v1/vpcs/{vpc_id}/subnets/{subnet_id}" and
// path "/v1/vpcs/vpc_123/subnets/subnet_456", returns ("vpc_123", "subnet_456").
func extractTwoPathParams(path string, prefix string, middle string) (string, string) {
	if !strings.HasPrefix(path, prefix) {
		return "", ""
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimPrefix(rest, "/")

	// First param
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return rest, ""
	}
	first := rest[:idx]
	rest = rest[idx+1:]

	// Skip middle segment
	if !strings.HasPrefix(rest, middle) {
		return first, ""
	}
	rest = strings.TrimPrefix(rest, middle)
	rest = strings.TrimPrefix(rest, "/")

	// Second param
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return first, rest[:idx]
	}
	return first, rest
}
