package db

// network_repo.go — VPC, Subnet, SecurityGroup, SecurityGroupRule, NetworkInterface,
// and InternetGateway persistence methods.
//
// Source: P2_VPC_NETWORK_CONTRACT §2.5 (vpcs schema), §3.3 (subnets schema),
//         §4.6 (security_groups, security_group_rules schemas), §5.4 (nics schema).
// VM-P3A Job 1: Extended VPCRow, SubnetRow, NetworkInterfaceRow with IPv6 fields;
//               added InternetGatewayRow and CRUD methods.
// Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate",
//         vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity".

import (
	"context"
	"fmt"
	"time"
)

// ── VPC ───────────────────────────────────────────────────────────────────────

// VPCRow is the DB representation of a VPC record.
// Source: P2_VPC_NETWORK_CONTRACT §2.5.
// VM-P3A Job 1: Added CIDRIPv6 (nullable; nil for IPv4-only VPCs).
type VPCRow struct {
	ID               string
	OwnerPrincipalID string
	Name             string
	CIDRIPv4         string
	CIDRIPv6         *string // nil = IPv4-only VPC
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// CreateVPC inserts a new VPC record.
// Source: P2_VPC_NETWORK_CONTRACT §2.5.
func (r *Repo) CreateVPC(ctx context.Context, row *VPCRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO vpcs (
			id, owner_principal_id, name, cidr_ipv4, cidr_ipv6, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
	`,
		row.ID, row.OwnerPrincipalID, row.Name, row.CIDRIPv4, row.CIDRIPv6, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateVPC: %w", err)
	}
	return nil
}

// GetVPCByID fetches a single VPC by its primary key.
// Returns nil, nil when no matching row exists.
// Source: P2_VPC_NETWORK_CONTRACT §2.5.
func (r *Repo) GetVPCByID(ctx context.Context, id string) (*VPCRow, error) {
	row := &VPCRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_principal_id, name, cidr_ipv4, cidr_ipv6, status, created_at, updated_at, deleted_at
		FROM vpcs
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.OwnerPrincipalID, &row.Name, &row.CIDRIPv4, &row.CIDRIPv6,
		&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetVPCByID: %w", err)
	}
	return row, nil
}

// ListVPCsByOwner returns all non-deleted VPCs for a given owner principal.
// Source: P2_VPC_NETWORK_CONTRACT §2.5.
func (r *Repo) ListVPCsByOwner(ctx context.Context, ownerPrincipalID string) ([]*VPCRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_principal_id, name, cidr_ipv4, cidr_ipv6, status, created_at, updated_at, deleted_at
		FROM vpcs
		WHERE owner_principal_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`, ownerPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("ListVPCsByOwner: %w", err)
	}
	defer rows.Close()

	var out []*VPCRow
	for rows.Next() {
		row := &VPCRow{}
		if err := rows.Scan(
			&row.ID, &row.OwnerPrincipalID, &row.Name, &row.CIDRIPv4, &row.CIDRIPv6,
			&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListVPCsByOwner scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SoftDeleteVPC marks a VPC as deleted by setting deleted_at.
// Returns error if VPC not found (0 rows affected).
// Source: P2_VPC_NETWORK_CONTRACT §2.6 VPC-I-3.
func (r *Repo) SoftDeleteVPC(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE vpcs
		SET deleted_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("SoftDeleteVPC: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteVPC: vpc %s not found or already deleted", id)
	}
	return nil
}

// ── Subnet ────────────────────────────────────────────────────────────────────

// SubnetRow is the DB representation of a Subnet record.
// Source: P2_VPC_NETWORK_CONTRACT §3.3.
// VM-P3A Job 1: Added CIDRIPv6 (nullable; nil for IPv4-only subnets).
type SubnetRow struct {
	ID               string
	VPCID            string
	Name             string
	CIDRIPv4         string
	CIDRIPv6         *string // nil = IPv4-only subnet
	AvailabilityZone string
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// CreateSubnet inserts a new Subnet record.
// Source: P2_VPC_NETWORK_CONTRACT §3.3.
func (r *Repo) CreateSubnet(ctx context.Context, row *SubnetRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO subnets (
			id, vpc_id, name, cidr_ipv4, cidr_ipv6, availability_zone, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
	`,
		row.ID, row.VPCID, row.Name, row.CIDRIPv4, row.CIDRIPv6, row.AvailabilityZone, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateSubnet: %w", err)
	}
	return nil
}

// GetSubnetByID fetches a single Subnet by its primary key.
// Returns nil, nil when no matching row exists.
// Source: P2_VPC_NETWORK_CONTRACT §3.3.
func (r *Repo) GetSubnetByID(ctx context.Context, id string) (*SubnetRow, error) {
	row := &SubnetRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, name, cidr_ipv4, cidr_ipv6, availability_zone, status, created_at, updated_at, deleted_at
		FROM subnets
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.VPCID, &row.Name, &row.CIDRIPv4, &row.CIDRIPv6,
		&row.AvailabilityZone, &row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSubnetByID: %w", err)
	}
	return row, nil
}

// ListSubnetsByVPC returns all non-deleted Subnets for a given VPC.
// Source: P2_VPC_NETWORK_CONTRACT §3.3.
func (r *Repo) ListSubnetsByVPC(ctx context.Context, vpcID string) ([]*SubnetRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, vpc_id, name, cidr_ipv4, cidr_ipv6, availability_zone, status, created_at, updated_at, deleted_at
		FROM subnets
		WHERE vpc_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`, vpcID)
	if err != nil {
		return nil, fmt.Errorf("ListSubnetsByVPC: %w", err)
	}
	defer rows.Close()

	var out []*SubnetRow
	for rows.Next() {
		row := &SubnetRow{}
		if err := rows.Scan(
			&row.ID, &row.VPCID, &row.Name, &row.CIDRIPv4, &row.CIDRIPv6,
			&row.AvailabilityZone, &row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListSubnetsByVPC scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SoftDeleteSubnet marks a Subnet as deleted by setting deleted_at.
// Returns error if Subnet not found (0 rows affected).
// Source: P2_VPC_NETWORK_CONTRACT §3.4 SUBNET-I-3.
func (r *Repo) SoftDeleteSubnet(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE subnets
		SET deleted_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("SoftDeleteSubnet: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteSubnet: subnet %s not found or already deleted", id)
	}
	return nil
}

// ── SecurityGroup ─────────────────────────────────────────────────────────────

// SecurityGroupRow is the DB representation of a SecurityGroup record.
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
type SecurityGroupRow struct {
	ID               string
	VPCID            string
	OwnerPrincipalID string
	Name             string
	Description      *string
	IsDefault        bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// CreateSecurityGroup inserts a new SecurityGroup record.
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
func (r *Repo) CreateSecurityGroup(ctx context.Context, row *SecurityGroupRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO security_groups (
			id, vpc_id, owner_principal_id, name, description, is_default, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
	`,
		row.ID, row.VPCID, row.OwnerPrincipalID, row.Name, row.Description, row.IsDefault,
	)
	if err != nil {
		return fmt.Errorf("CreateSecurityGroup: %w", err)
	}
	return nil
}

// GetSecurityGroupByID fetches a single SecurityGroup by its primary key.
// Returns nil, nil when no matching row exists.
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
func (r *Repo) GetSecurityGroupByID(ctx context.Context, id string) (*SecurityGroupRow, error) {
	row := &SecurityGroupRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, owner_principal_id, name, description, is_default,
		       created_at, updated_at, deleted_at
		FROM security_groups
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.VPCID, &row.OwnerPrincipalID, &row.Name, &row.Description, &row.IsDefault,
		&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSecurityGroupByID: %w", err)
	}
	return row, nil
}

// ListSecurityGroupsByVPC returns all non-deleted SecurityGroups for a given VPC.
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
func (r *Repo) ListSecurityGroupsByVPC(ctx context.Context, vpcID string) ([]*SecurityGroupRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, vpc_id, owner_principal_id, name, description, is_default,
		       created_at, updated_at, deleted_at
		FROM security_groups
		WHERE vpc_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`, vpcID)
	if err != nil {
		return nil, fmt.Errorf("ListSecurityGroupsByVPC: %w", err)
	}
	defer rows.Close()

	var out []*SecurityGroupRow
	for rows.Next() {
		row := &SecurityGroupRow{}
		if err := rows.Scan(
			&row.ID, &row.VPCID, &row.OwnerPrincipalID, &row.Name, &row.Description, &row.IsDefault,
			&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListSecurityGroupsByVPC scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── SecurityGroupRule ─────────────────────────────────────────────────────────

// SecurityGroupRuleRow is the DB representation of a SecurityGroupRule record.
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
type SecurityGroupRuleRow struct {
	ID                    string
	SecurityGroupID       string
	Direction             string // 'ingress' | 'egress'
	Protocol              string // 'tcp' | 'udp' | 'icmp' | 'all'
	PortFrom              *int
	PortTo                *int
	CIDR                  *string
	SourceSecurityGroupID *string
	CreatedAt             time.Time
}

// CreateSecurityGroupRule inserts a new SecurityGroupRule record.
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
func (r *Repo) CreateSecurityGroupRule(ctx context.Context, row *SecurityGroupRuleRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO security_group_rules (
			id, security_group_id, direction, protocol, port_from, port_to,
			cidr, source_sg_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
	`,
		row.ID, row.SecurityGroupID, row.Direction, row.Protocol,
		row.PortFrom, row.PortTo, row.CIDR, row.SourceSecurityGroupID,
	)
	if err != nil {
		return fmt.Errorf("CreateSecurityGroupRule: %w", err)
	}
	return nil
}

// DeleteSecurityGroupRule removes a SecurityGroupRule by its primary key.
// Returns error if rule not found (0 rows affected).
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
func (r *Repo) DeleteSecurityGroupRule(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM security_group_rules WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("DeleteSecurityGroupRule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DeleteSecurityGroupRule: rule %s not found", id)
	}
	return nil
}

// ListSecurityGroupRulesBySecurityGroup returns all rules for a given SecurityGroup.
// Source: P2_VPC_NETWORK_CONTRACT §4.6.
func (r *Repo) ListSecurityGroupRulesBySecurityGroup(ctx context.Context, securityGroupID string) ([]*SecurityGroupRuleRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, security_group_id, direction, protocol, port_from, port_to,
		       cidr, source_sg_id, created_at
		FROM security_group_rules
		WHERE security_group_id = $1
		ORDER BY created_at ASC
	`, securityGroupID)
	if err != nil {
		return nil, fmt.Errorf("ListSecurityGroupRulesBySecurityGroup: %w", err)
	}
	defer rows.Close()

	var out []*SecurityGroupRuleRow
	for rows.Next() {
		row := &SecurityGroupRuleRow{}
		if err := rows.Scan(
			&row.ID, &row.SecurityGroupID, &row.Direction, &row.Protocol,
			&row.PortFrom, &row.PortTo, &row.CIDR, &row.SourceSecurityGroupID, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListSecurityGroupRulesBySecurityGroup scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── NetworkInterface ──────────────────────────────────────────────────────────

// NetworkInterfaceRow is the DB representation of a NetworkInterface (NIC) record.
// Source: P2_VPC_NETWORK_CONTRACT §5.4.
// VM-P3A Job 1: Added PrivateIPv6 (nullable) and DeviceIndex.
type NetworkInterfaceRow struct {
	ID          string
	InstanceID  string
	SubnetID    string
	VPCID       string
	PrivateIP   string
	PrivateIPv6 *string // nil = IPv4-only NIC
	MACAddress  string
	DeviceIndex int
	IsPrimary   bool
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// CreateNetworkInterface inserts a new NetworkInterface record.
// Source: P2_VPC_NETWORK_CONTRACT §5.4.
func (r *Repo) CreateNetworkInterface(ctx context.Context, row *NetworkInterfaceRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO network_interfaces (
			id, instance_id, subnet_id, vpc_id, private_ip, private_ipv6, mac_address,
			device_index, is_primary, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
	`,
		row.ID, row.InstanceID, row.SubnetID, row.VPCID, row.PrivateIP, row.PrivateIPv6,
		row.MACAddress, row.DeviceIndex, row.IsPrimary, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateNetworkInterface: %w", err)
	}
	return nil
}

// GetNetworkInterfaceByID fetches a single NetworkInterface by its primary key.
// Returns nil, nil when no matching row exists.
// Source: P2_VPC_NETWORK_CONTRACT §5.4.
func (r *Repo) GetNetworkInterfaceByID(ctx context.Context, id string) (*NetworkInterfaceRow, error) {
	row := &NetworkInterfaceRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, subnet_id, vpc_id, private_ip, private_ipv6, mac_address,
		       device_index, is_primary, status, created_at, updated_at, deleted_at
		FROM network_interfaces
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.InstanceID, &row.SubnetID, &row.VPCID, &row.PrivateIP, &row.PrivateIPv6,
		&row.MACAddress, &row.DeviceIndex, &row.IsPrimary, &row.Status,
		&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetNetworkInterfaceByID: %w", err)
	}
	return row, nil
}

// ListNetworkInterfacesByInstance returns all non-deleted NetworkInterfaces for a given instance.
// Source: P2_VPC_NETWORK_CONTRACT §5.4.
func (r *Repo) ListNetworkInterfacesByInstance(ctx context.Context, instanceID string) ([]*NetworkInterfaceRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, instance_id, subnet_id, vpc_id, private_ip, private_ipv6, mac_address,
		       device_index, is_primary, status, created_at, updated_at, deleted_at
		FROM network_interfaces
		WHERE instance_id = $1
		  AND deleted_at IS NULL
		ORDER BY is_primary DESC, device_index ASC, created_at ASC
	`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("ListNetworkInterfacesByInstance: %w", err)
	}
	defer rows.Close()

	var out []*NetworkInterfaceRow
	for rows.Next() {
		row := &NetworkInterfaceRow{}
		if err := rows.Scan(
			&row.ID, &row.InstanceID, &row.SubnetID, &row.VPCID, &row.PrivateIP, &row.PrivateIPv6,
			&row.MACAddress, &row.DeviceIndex, &row.IsPrimary, &row.Status,
			&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListNetworkInterfacesByInstance scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── InternetGateway ───────────────────────────────────────────────────────────

// InternetGatewayRow is the DB representation of an InternetGateway record.
// VM-P3A Job 1: New first-class resource. Required for IGW Exclusivity contract.
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity",
//         §implementation_decisions "explicit InternetGateway resource".
type InternetGatewayRow struct {
	ID               string
	VPCID            string
	OwnerPrincipalID string
	Status           string // 'available' | 'deleting' | 'deleted'
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// CreateInternetGateway inserts a new InternetGateway record.
// The unique partial index (idx_internet_gateways_vpc_active) enforces one-per-VPC.
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity".
func (r *Repo) CreateInternetGateway(ctx context.Context, row *InternetGatewayRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO internet_gateways (
			id, vpc_id, owner_principal_id, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, NOW(), NOW())
	`,
		row.ID, row.VPCID, row.OwnerPrincipalID, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateInternetGateway: %w", err)
	}
	return nil
}

// GetInternetGatewayByID fetches a single InternetGateway by its primary key.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetInternetGatewayByID(ctx context.Context, id string) (*InternetGatewayRow, error) {
	row := &InternetGatewayRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, owner_principal_id, status, created_at, updated_at, deleted_at
		FROM internet_gateways
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.VPCID, &row.OwnerPrincipalID, &row.Status,
		&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetInternetGatewayByID: %w", err)
	}
	return row, nil
}

// GetInternetGatewayByVPC returns the active InternetGateway for a VPC, if any.
// Returns nil, nil when no active IGW is attached to the VPC.
// Source: vm-14-03__blueprint__ §core_contracts "Internet Gateway Exclusivity".
func (r *Repo) GetInternetGatewayByVPC(ctx context.Context, vpcID string) (*InternetGatewayRow, error) {
	row := &InternetGatewayRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, owner_principal_id, status, created_at, updated_at, deleted_at
		FROM internet_gateways
		WHERE vpc_id = $1
		  AND deleted_at IS NULL
		LIMIT 1
	`, vpcID).Scan(
		&row.ID, &row.VPCID, &row.OwnerPrincipalID, &row.Status,
		&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetInternetGatewayByVPC: %w", err)
	}
	return row, nil
}

// ListInternetGatewaysByOwner returns all non-deleted InternetGateways for a given owner.
func (r *Repo) ListInternetGatewaysByOwner(ctx context.Context, ownerPrincipalID string) ([]*InternetGatewayRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, vpc_id, owner_principal_id, status, created_at, updated_at, deleted_at
		FROM internet_gateways
		WHERE owner_principal_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`, ownerPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("ListInternetGatewaysByOwner: %w", err)
	}
	defer rows.Close()

	var out []*InternetGatewayRow
	for rows.Next() {
		row := &InternetGatewayRow{}
		if err := rows.Scan(
			&row.ID, &row.VPCID, &row.OwnerPrincipalID, &row.Status,
			&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListInternetGatewaysByOwner scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SoftDeleteInternetGateway marks an InternetGateway as deleted.
// Returns error if not found or already deleted (0 rows affected).
func (r *Repo) SoftDeleteInternetGateway(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE internet_gateways
		SET deleted_at = NOW(),
		    updated_at = NOW(),
		    status     = 'deleted'
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("SoftDeleteInternetGateway: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteInternetGateway: igw %s not found or already deleted", id)
	}
	return nil
}
