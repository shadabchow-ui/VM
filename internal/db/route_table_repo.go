package db

// route_table_repo.go — RouteTable, RouteEntry, and SubnetRouteTableAssociation persistence.
//
// Source: P2_VPC_NETWORK_CONTRACT §11 P2-VPC-OD-3 (route table model).
// Phase 2 M9 Slice 1: Minimal route table control-plane foundation.
// VM-P3A Job 1: Extended RouteEntryRow with AddressFamily; added validation
//               helpers ValidateIGWExclusivity and ValidateRouteLoopFree.
//
// Phase 2 uses implicit single-default route table per VPC for simplicity.
// Schema supports explicit route tables for future extensibility.
//
// VM-P3A route-table contracts enforced here:
//   IGW Exclusivity: a 0.0.0.0/0 or ::/0 route may only target an IGW that is
//     explicitly attached to the route table's parent VPC.
//   NAT Anti-Loop: a 0.0.0.0/0 route targeting a NatGateway must not reside in
//     the same subnet as the NAT Gateway itself.
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity",
//         "Gateway Default Route Target", "NAT Gateway Anti-Loop".

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
// VM-P3A Job 1: Added AddressFamily to distinguish IPv4, IPv6, and dual-stack routes.
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

// ── Route Validation Helpers ─────────────────────────────────────────────────
//
// These helpers are called by the resource-manager handler before persisting a
// new route entry. They enforce the vm-14-03 blueprint contracts at the DB layer
// so the validation is close to the data and can be reused by future workers or
// reconciler paths without duplicating SQL.

// IGWExclusivityError is returned when a default route targets an IGW that is not
// attached to the route table's parent VPC.
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity".
type IGWExclusivityError struct {
	VPCID string
	IGWID string
}

func (e *IGWExclusivityError) Error() string {
	return fmt.Sprintf("internet gateway %s is not attached to vpc %s", e.IGWID, e.VPCID)
}

// ValidateIGWExclusivity checks that a proposed route entry does not violate the
// Internet Gateway Exclusivity contract:
//   - A default route (0.0.0.0/0 or ::/0) with target_type='igw' is only valid
//     when the referenced IGW is attached to the route table's parent VPC.
//
// Call this before CreateRouteEntry for any igw-targeted route.
// Returns nil when the proposed entry is valid.
// Returns *IGWExclusivityError when the exclusivity rule is violated.
//
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity".
func (r *Repo) ValidateIGWExclusivity(ctx context.Context, routeTableID, igwID string) error {
	// Resolve the VPC that owns this route table.
	var vpcID string
	err := r.pool.QueryRow(ctx, `
		SELECT vpc_id FROM route_tables WHERE id = $1 AND deleted_at IS NULL
	`, routeTableID).Scan(&vpcID)
	if err != nil {
		if isNoRowsErr(err) {
			return fmt.Errorf("ValidateIGWExclusivity: route table %s not found", routeTableID)
		}
		return fmt.Errorf("ValidateIGWExclusivity: lookup route table: %w", err)
	}

	// Verify the IGW exists and is attached to this VPC (not deleted).
	var count int
	err = r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM internet_gateways
		WHERE id = $1
		  AND vpc_id = $2
		  AND deleted_at IS NULL
	`, igwID, vpcID).Scan(&count)
	if err != nil {
		return fmt.Errorf("ValidateIGWExclusivity: lookup igw: %w", err)
	}
	if count == 0 {
		return &IGWExclusivityError{VPCID: vpcID, IGWID: igwID}
	}
	return nil
}

// NATLoopError is returned when a NAT-targeted default route would create a routing loop.
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Anti-Loop".
type NATLoopError struct {
	RouteTableID string
	SubnetID     string
}

func (e *NATLoopError) Error() string {
	return fmt.Sprintf(
		"routing loop: route table %s is associated with subnet %s, which is the same subnet as the nat gateway target",
		e.RouteTableID, e.SubnetID,
	)
}

// ValidateRouteLoopFree checks that a proposed route entry with target_type='nat'
// does not create a NAT routing loop:
//   - A route table associated with subnet S cannot have a default route
//     targeting a NAT Gateway located in subnet S.
//
// natGatewaySubnetID is the subnet in which the NAT Gateway resource resides.
// Call this before CreateRouteEntry for any nat-targeted default route.
// Returns nil when the proposed entry is valid.
// Returns *NATLoopError when a loop would be created.
//
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Anti-Loop".
func (r *Repo) ValidateRouteLoopFree(ctx context.Context, routeTableID, natGatewaySubnetID string) error {
	// Find all subnets associated with this route table (explicit associations).
	rows, err := r.pool.Query(ctx, `
		SELECT subnet_id
		FROM subnet_route_table_associations
		WHERE route_table_id = $1
	`, routeTableID)
	if err != nil {
		return fmt.Errorf("ValidateRouteLoopFree: list associations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var subnetID string
		if err := rows.Scan(&subnetID); err != nil {
			return fmt.Errorf("ValidateRouteLoopFree scan: %w", err)
		}
		if subnetID == natGatewaySubnetID {
			return &NATLoopError{RouteTableID: routeTableID, SubnetID: subnetID}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ValidateRouteLoopFree rows: %w", err)
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

// ListSubnetsByRouteTable returns all subnet IDs explicitly associated with a route table.
// Used by ValidateRouteLoopFree and future reconciler drift checks.
func (r *Repo) ListSubnetsByRouteTable(ctx context.Context, routeTableID string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT subnet_id
		FROM subnet_route_table_associations
		WHERE route_table_id = $1
		ORDER BY created_at ASC
	`, routeTableID)
	if err != nil {
		return nil, fmt.Errorf("ListSubnetsByRouteTable: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("ListSubnetsByRouteTable scan: %w", err)
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}
