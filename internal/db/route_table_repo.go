package db

// route_table_repo.go — RouteTable and RouteEntry persistence methods.
//
// Source: P2_VPC_NETWORK_CONTRACT §11 P2-VPC-OD-3 (route table model).
// Phase 2 M9 Slice 1: Minimal route table control-plane foundation.
//
// VM-P3A Job 1 additions:
//   - RouteEntryRow.AddressFamily field ('ipv4' | 'ipv6' | 'dual').
//   - CreateRouteEntry: includes address_family in INSERT; defaults to 'ipv4'.
//   - ListRouteEntriesByRouteTable: scans address_family.
//   - IGWExclusivityError: returned when an igw-targeted route's IGW is not
//     attached to the route table's parent VPC.
//   - NATLoopError: returned when a nat-targeted route would create a loop.
//   - ValidateIGWExclusivity: pre-INSERT check for IGW Exclusivity contract.
//   - ValidateRouteLoopFree: pre-INSERT check for NAT Anti-Loop contract.
//
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity",
//         "NAT Gateway Anti-Loop".

import (
	"context"
	"fmt"
	"time"
)

// ── RouteTable ───────────────────────────────────────────────────────────────

// RouteTableRow is the DB representation of a RouteTable record.
type RouteTableRow struct {
	ID        string
	VPCID     string
	Name      string
	IsDefault bool
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// CreateRouteTable inserts a new RouteTable record.
func (r *Repo) CreateRouteTable(ctx context.Context, row *RouteTableRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO route_tables (
			id, vpc_id, name, is_default, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
	`,
		row.ID, row.VPCID, row.Name, row.IsDefault, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateRouteTable: %w", err)
	}
	return nil
}

// GetRouteTableByID fetches a single RouteTable by its primary key.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetRouteTableByID(ctx context.Context, id string) (*RouteTableRow, error) {
	row := &RouteTableRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, name, is_default, status, created_at, updated_at, deleted_at
		FROM route_tables
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.VPCID, &row.Name, &row.IsDefault,
		&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetRouteTableByID: %w", err)
	}
	return row, nil
}

// GetDefaultRouteTableByVPC returns the default route table for a VPC.
// Returns nil, nil if no default exists.
func (r *Repo) GetDefaultRouteTableByVPC(ctx context.Context, vpcID string) (*RouteTableRow, error) {
	row := &RouteTableRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, name, is_default, status, created_at, updated_at, deleted_at
		FROM route_tables
		WHERE vpc_id = $1
		  AND is_default = TRUE
		  AND deleted_at IS NULL
	`, vpcID).Scan(
		&row.ID, &row.VPCID, &row.Name, &row.IsDefault,
		&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetDefaultRouteTableByVPC: %w", err)
	}
	return row, nil
}

// ListRouteTablesByVPC returns all non-deleted RouteTables for a given VPC.
func (r *Repo) ListRouteTablesByVPC(ctx context.Context, vpcID string) ([]*RouteTableRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, vpc_id, name, is_default, status, created_at, updated_at, deleted_at
		FROM route_tables
		WHERE vpc_id = $1
		  AND deleted_at IS NULL
		ORDER BY is_default DESC, created_at ASC
	`, vpcID)
	if err != nil {
		return nil, fmt.Errorf("ListRouteTablesByVPC: %w", err)
	}
	defer rows.Close()

	var out []*RouteTableRow
	for rows.Next() {
		row := &RouteTableRow{}
		if err := rows.Scan(
			&row.ID, &row.VPCID, &row.Name, &row.IsDefault,
			&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListRouteTablesByVPC scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SoftDeleteRouteTable marks a RouteTable as deleted.
// Returns error if not found or is_default=true (cannot delete default route table).
func (r *Repo) SoftDeleteRouteTable(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE route_tables
		SET deleted_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
		  AND is_default = FALSE
	`, id)
	if err != nil {
		return fmt.Errorf("SoftDeleteRouteTable: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteRouteTable: route table %s not found, already deleted, or is default", id)
	}
	return nil
}

// ── RouteEntry ───────────────────────────────────────────────────────────────

// RouteEntryRow is the DB representation of a RouteEntry record.
// VM-P3A Job 1: Added AddressFamily to distinguish IPv4/IPv6/dual-stack routes.
// Source: vm-14-03__blueprint__ §future_phases "IPv6 Integration".
type RouteEntryRow struct {
	ID              string
	RouteTableID    string
	DestinationCIDR string
	TargetType      string  // 'local', 'igw', 'nat', 'peering'
	TargetID        *string // NULL for 'local'
	AddressFamily   string  // 'ipv4' | 'ipv6' | 'dual' — VM-P3A Job 1
	Priority        int
	Status          string
	CreatedAt       time.Time
}

// CreateRouteEntry inserts a new RouteEntry record.
// AddressFamily defaults to 'ipv4' when not set by caller.
// Source: vm-14-03__blueprint__ §core_contracts "Route Entry Model".
func (r *Repo) CreateRouteEntry(ctx context.Context, row *RouteEntryRow) error {
	af := row.AddressFamily
	if af == "" {
		af = "ipv4"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO route_entries (
			id, route_table_id, destination_cidr, target_type, target_id,
			address_family, priority, status, created_at
		) VALUES ($1, $2, $3::CIDR, $4, $5, $6, $7, $8, NOW())
	`,
		row.ID, row.RouteTableID, row.DestinationCIDR, row.TargetType,
		row.TargetID, af, row.Priority, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateRouteEntry: %w", err)
	}
	return nil
}

// ListRouteEntriesByRouteTable returns all route entries for a given route table.
func (r *Repo) ListRouteEntriesByRouteTable(ctx context.Context, routeTableID string) ([]*RouteEntryRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, route_table_id, destination_cidr::TEXT, target_type, target_id,
		       address_family, priority, status, created_at
		FROM route_entries
		WHERE route_table_id = $1
		ORDER BY priority ASC, created_at ASC
	`, routeTableID)
	if err != nil {
		return nil, fmt.Errorf("ListRouteEntriesByRouteTable: %w", err)
	}
	defer rows.Close()

	var out []*RouteEntryRow
	for rows.Next() {
		row := &RouteEntryRow{}
		if err := rows.Scan(
			&row.ID, &row.RouteTableID, &row.DestinationCIDR, &row.TargetType,
			&row.TargetID, &row.AddressFamily, &row.Priority, &row.Status, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListRouteEntriesByRouteTable scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// DeleteRouteEntry removes a route entry by its primary key.
// Returns error if not found.
func (r *Repo) DeleteRouteEntry(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM route_entries WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("DeleteRouteEntry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DeleteRouteEntry: route entry %s not found", id)
	}
	return nil
}

// ── SubnetRouteTableAssociation ──────────────────────────────────────────────

// SubnetRouteTableAssociationRow is the DB representation of a subnet-route-table association.
type SubnetRouteTableAssociationRow struct {
	ID           string
	SubnetID     string
	RouteTableID string
	CreatedAt    time.Time
}

// GetSubnetRouteTableAssociation returns the route table association for a subnet.
// Returns nil, nil if no explicit association exists (uses VPC default).
func (r *Repo) GetSubnetRouteTableAssociation(ctx context.Context, subnetID string) (*SubnetRouteTableAssociationRow, error) {
	row := &SubnetRouteTableAssociationRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, subnet_id, route_table_id, created_at
		FROM subnet_route_table_associations
		WHERE subnet_id = $1
	`, subnetID).Scan(
		&row.ID, &row.SubnetID, &row.RouteTableID, &row.CreatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSubnetRouteTableAssociation: %w", err)
	}
	return row, nil
}

// SetSubnetRouteTableAssociation sets or replaces a subnet's route table association.
// Uses UPSERT pattern.
func (r *Repo) SetSubnetRouteTableAssociation(ctx context.Context, row *SubnetRouteTableAssociationRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO subnet_route_table_associations (id, subnet_id, route_table_id, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (subnet_id) DO UPDATE
		SET route_table_id = EXCLUDED.route_table_id
	`, row.ID, row.SubnetID, row.RouteTableID)
	if err != nil {
		return fmt.Errorf("SetSubnetRouteTableAssociation: %w", err)
	}
	return nil
}

// DeleteSubnetRouteTableAssociation removes a subnet's explicit route table association.
// After deletion, subnet uses the VPC's default route table.
func (r *Repo) DeleteSubnetRouteTableAssociation(ctx context.Context, subnetID string) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM subnet_route_table_associations WHERE subnet_id = $1
	`, subnetID)
	if err != nil {
		return fmt.Errorf("DeleteSubnetRouteTableAssociation: %w", err)
	}
	return nil
}

// ── Route Validation Error Types — VM-P3A Job 1 ───────────────────────────────

// IGWExclusivityError is returned by ValidateIGWExclusivity when a proposed
// default route targets an Internet Gateway that is not attached to the VPC
// that owns the route table.
//
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity":
//   "A route table may only reference an IGW that is attached to its parent VPC."
type IGWExclusivityError struct {
	VPCID string
	IGWID string
}

func (e *IGWExclusivityError) Error() string {
	return fmt.Sprintf("internet gateway %s is not attached to vpc %s", e.IGWID, e.VPCID)
}

// NATLoopError is returned by ValidateRouteLoopFree when a proposed default
// route targeting a NAT Gateway would create a routing loop: the NAT Gateway's
// subnet is itself associated with (or defaults to) the route table being modified.
//
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Anti-Loop":
//   "A 0.0.0.0/0 → NAT route must not be in the same route table used by the
//    subnet hosting that NAT gateway."
type NATLoopError struct {
	RouteTableID string
	SubnetID     string
}

func (e *NATLoopError) Error() string {
	return fmt.Sprintf("route table %s is used by subnet %s which hosts the NAT gateway — routing loop", e.RouteTableID, e.SubnetID)
}

// ── Route Validation Helpers — VM-P3A Job 1 ──────────────────────────────────

// ValidateIGWExclusivity checks that the Internet Gateway identified by igwID
// is attached to the VPC that owns the route table identified by routeTableID.
//
// Returns *IGWExclusivityError when the exclusivity rule is violated.
// Returns nil when the IGW is in the correct VPC (route entry is safe to create).
//
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity".
func (r *Repo) ValidateIGWExclusivity(ctx context.Context, routeTableID, igwID string) error {
	// Fetch the route table's parent VPC.
	rtb, err := r.GetRouteTableByID(ctx, routeTableID)
	if err != nil {
		return fmt.Errorf("ValidateIGWExclusivity: lookup route table: %w", err)
	}
	if rtb == nil {
		return fmt.Errorf("ValidateIGWExclusivity: route table %s not found", routeTableID)
	}
	vpcID := rtb.VPCID

	// Fetch the IGW and check its parent VPC.
	igw, err := r.GetInternetGatewayByID(ctx, igwID)
	if err != nil {
		return fmt.Errorf("ValidateIGWExclusivity: lookup igw: %w", err)
	}
	if igw == nil || igw.VPCID != vpcID {
		return &IGWExclusivityError{VPCID: vpcID, IGWID: igwID}
	}
	return nil
}

// ValidateRouteLoopFree checks that adding a default route targeting a NAT
// Gateway in natGatewaySubnetID would not create a routing loop.
//
// A loop occurs when natGatewaySubnetID is associated with routeTableID (either
// explicitly via subnet_route_table_associations, or implicitly when the subnet's
// VPC uses routeTableID as its default route table).
//
// Returns *NATLoopError when a loop would be created.
// Returns nil when the route is safe to create.
//
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Anti-Loop".
func (r *Repo) ValidateRouteLoopFree(ctx context.Context, routeTableID, natGatewaySubnetID string) error {
	// Check explicit association first.
	assoc, err := r.GetSubnetRouteTableAssociation(ctx, natGatewaySubnetID)
	if err != nil {
		return fmt.Errorf("ValidateRouteLoopFree: check explicit assoc: %w", err)
	}
	if assoc != nil {
		// Subnet has an explicit association — loop if it points to our route table.
		if assoc.RouteTableID == routeTableID {
			return &NATLoopError{RouteTableID: routeTableID, SubnetID: natGatewaySubnetID}
		}
		// Explicit association points elsewhere — no loop.
		return nil
	}

	// No explicit association — subnet uses its VPC's default route table.
	// Fetch the subnet to get its VPC, then check if our route table is that VPC's default.
	subnet, err := r.GetSubnetByID(ctx, natGatewaySubnetID)
	if err != nil {
		return fmt.Errorf("ValidateRouteLoopFree: lookup subnet: %w", err)
	}
	if subnet == nil {
		// Subnet not found — cannot validate; allow the route (caller validates subnet existence separately).
		return nil
	}

	defaultRTB, err := r.GetDefaultRouteTableByVPC(ctx, subnet.VPCID)
	if err != nil {
		return fmt.Errorf("ValidateRouteLoopFree: get default rtb: %w", err)
	}
	if defaultRTB != nil && defaultRTB.ID == routeTableID {
		return &NATLoopError{RouteTableID: routeTableID, SubnetID: natGatewaySubnetID}
	}
	return nil
}
