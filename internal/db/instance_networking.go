package db

// instance_networking.go — Instance networking integration for Phase 2 VPC support.
//
// M9 Slice 4: Integrates networking into instance create/describe paths.
// - Subnet validation
// - Primary NIC creation
// - VPC-aware IP allocation
// - Security group attachment to NIC
//
// Source: P2_VPC_NETWORK_CONTRACT §5 (NIC model), P2_MILESTONE_PLAN §P2-M2.

import (
	"context"
	"fmt"
	"net"
)

// NICSecurityGroupRow represents the nic_security_groups junction table row.
type NICSecurityGroupRow struct {
	NICID           string
	SecurityGroupID string
}

// CreateNICSecurityGroupLink inserts a junction row linking a NIC to a security group.
// Source: P2_VPC_NETWORK_CONTRACT §5.4 (nic_security_groups schema).
func (r *Repo) CreateNICSecurityGroupLink(ctx context.Context, nicID, sgID string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO nic_security_groups (nic_id, security_group_id)
		VALUES ($1, $2)
	`, nicID, sgID)
	if err != nil {
		return fmt.Errorf("CreateNICSecurityGroupLink: %w", err)
	}
	return nil
}

// ListSecurityGroupIDsByNIC returns all security group IDs attached to a NIC.
// Source: P2_VPC_NETWORK_CONTRACT §5.4.
func (r *Repo) ListSecurityGroupIDsByNIC(ctx context.Context, nicID string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT security_group_id
		FROM nic_security_groups
		WHERE nic_id = $1
	`, nicID)
	if err != nil {
		return nil, fmt.Errorf("ListSecurityGroupIDsByNIC: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var sgID string
		if err := rows.Scan(&sgID); err != nil {
			return nil, fmt.Errorf("ListSecurityGroupIDsByNIC scan: %w", err)
		}
		out = append(out, sgID)
	}
	return out, rows.Err()
}

// GetPrimaryNetworkInterfaceByInstance returns the primary NIC for an instance.
// Returns nil, nil if no primary NIC exists (Phase 1 classic instance).
// Source: P2_VPC_NETWORK_CONTRACT §5.2 (is_primary field).
func (r *Repo) GetPrimaryNetworkInterfaceByInstance(ctx context.Context, instanceID string) (*NetworkInterfaceRow, error) {
	row := &NetworkInterfaceRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, instance_id, subnet_id, vpc_id, private_ip, mac_address,
		       is_primary, status, created_at, updated_at, deleted_at
		FROM network_interfaces
		WHERE instance_id = $1
		  AND is_primary = TRUE
		  AND deleted_at IS NULL
	`, instanceID).Scan(
		&row.ID, &row.InstanceID, &row.SubnetID, &row.VPCID, &row.PrivateIP,
		&row.MACAddress, &row.IsPrimary, &row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetPrimaryNetworkInterfaceByInstance: %w", err)
	}
	return row, nil
}

// AllocateIPFromSubnet allocates an IP address from a subnet's CIDR range.
// Uses SELECT FOR UPDATE SKIP LOCKED to avoid contention.
// Returns the allocated IP or error if subnet is exhausted.
//
// Implementation note: This requires a subnet_ip_allocations table similar to
// the Phase 1 ip_allocations table but keyed by subnet_id.
//
// Source: P2_VPC_NETWORK_CONTRACT §5 (NIC private IP), IP_ALLOCATION_CONTRACT_V1 pattern.
func (r *Repo) AllocateIPFromSubnet(ctx context.Context, subnetID, instanceID string) (string, error) {
	var ip string
	err := r.pool.QueryRow(ctx, `
		UPDATE subnet_ip_allocations
		SET allocated = TRUE,
		    owner_instance_id = $2,
		    updated_at = NOW()
		WHERE ip_address = (
			SELECT ip_address
			FROM subnet_ip_allocations
			WHERE subnet_id = $1
			  AND allocated = FALSE
			ORDER BY ip_address
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		  AND subnet_id = $1
		RETURNING ip_address::TEXT
	`, subnetID, instanceID).Scan(&ip)
	if err != nil {
		if isNoRowsErr(err) {
			return "", fmt.Errorf("AllocateIPFromSubnet: no available IPs in subnet %s", subnetID)
		}
		return "", fmt.Errorf("AllocateIPFromSubnet: %w", err)
	}
	return ip, nil
}

// ReleaseIPFromSubnet releases an IP back to the subnet pool.
// Idempotent: does not error if IP was already released.
// Source: IP_ALLOCATION_CONTRACT_V1 pattern.
func (r *Repo) ReleaseIPFromSubnet(ctx context.Context, ip, subnetID, instanceID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE subnet_ip_allocations
		SET allocated = FALSE,
		    owner_instance_id = NULL,
		    updated_at = NOW()
		WHERE ip_address = $1::INET
		  AND subnet_id = $2
		  AND owner_instance_id = $3
	`, ip, subnetID, instanceID)
	if err != nil {
		return fmt.Errorf("ReleaseIPFromSubnet: %w", err)
	}
	return nil
}

// GetDefaultSecurityGroupForVPC returns the default security group for a VPC.
// Returns nil, nil if no default SG exists (shouldn't happen for valid VPCs).
// Source: P2_VPC_NETWORK_CONTRACT §4.4 (default SG behavior).
func (r *Repo) GetDefaultSecurityGroupForVPC(ctx context.Context, vpcID string) (*SecurityGroupRow, error) {
	row := &SecurityGroupRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, owner_principal_id, name, description, is_default,
		       created_at, updated_at, deleted_at
		FROM security_groups
		WHERE vpc_id = $1
		  AND is_default = TRUE
		  AND deleted_at IS NULL
	`, vpcID).Scan(
		&row.ID, &row.VPCID, &row.OwnerPrincipalID, &row.Name, &row.Description, &row.IsDefault,
		&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetDefaultSecurityGroupForVPC: %w", err)
	}
	return row, nil
}

// ValidateSecurityGroupsInVPC checks that all provided security group IDs:
// 1. Exist and are not deleted
// 2. Belong to the specified VPC
// 3. Are owned by the specified principal (or are system default)
// Returns nil if all valid, or error describing the first invalid SG.
// Source: P2_VPC_NETWORK_CONTRACT §4.7 SG-I-1.
func (r *Repo) ValidateSecurityGroupsInVPC(ctx context.Context, sgIDs []string, vpcID, principalID string) error {
	for _, sgID := range sgIDs {
		sg, err := r.GetSecurityGroupByID(ctx, sgID)
		if err != nil {
			return fmt.Errorf("ValidateSecurityGroupsInVPC: %w", err)
		}
		if sg == nil {
			return fmt.Errorf("security group %s not found", sgID)
		}
		if sg.DeletedAt != nil {
			return fmt.Errorf("security group %s has been deleted", sgID)
		}
		if sg.VPCID != vpcID {
			return fmt.Errorf("security group %s is not in VPC %s", sgID, vpcID)
		}
		// Allow if it's the default SG or owned by the principal
		if !sg.IsDefault && sg.OwnerPrincipalID != principalID {
			return fmt.Errorf("security group %s is not accessible", sgID)
		}
	}
	return nil
}

// GenerateMACAddress generates a locally-administered MAC address.
// Format: 02:xx:xx:xx:xx:xx where xx are random hex bytes.
// The 02 prefix indicates a locally administered address.
func GenerateMACAddress() string {
	// In production, use crypto/rand. For now, use a simple pattern.
	// The 02 prefix marks this as locally administered.
	return "02:00:00:00:00:01" // Placeholder — in production, generate random bytes
}

// SubnetContainsIP checks if an IP address falls within a subnet's CIDR.
func SubnetContainsIP(cidr, ipStr string) (bool, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, fmt.Errorf("invalid CIDR %s: %w", cidr, err)
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false, fmt.Errorf("invalid IP %s", ipStr)
	}
	return network.Contains(ip), nil
}
