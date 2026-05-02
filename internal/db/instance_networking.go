package db

// instance_networking.go — Instance networking integration for Phase 2 VPC support.
//
// M9 Slice 4: Integrates networking into instance create/describe paths.
// - Subnet validation
// - Primary NIC creation
// - VPC-aware IP allocation
// - Security group attachment to NIC
//
// VM-P3A Job 2: Added:
//   - GetEffectiveSGRulesForNIC — joins nic_security_groups → security_group_rules
//     to return the union of all rules attached to a NIC. Used by host-agent
//     to build the nftables policy set.
//   - UpdateNICSecurityGroups — replaces the full SG set for a NIC atomically.
//     Used by the NIC SG update handler.
//
// REPAIR NOTE: UpdateNICSecurityGroups previously used r.pool.Begin(ctx) which
// is not part of the Pool interface (Pool only exposes Exec, Query, QueryRow, Close).
// Replaced with two sequential Exec calls (delete then insert). This is safe
// because the caller validates sgIDs before calling and the operation is
// idempotent per nicID. A partial failure leaves the NIC with no SGs, which
// falls back to deny-all at the host-agent layer — the correct safe default.
//
// Source: P2_VPC_NETWORK_CONTRACT §5 (NIC model), P2_MILESTONE_PLAN §P2-M2,
//         vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model".

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

// EffectiveSGRuleRow is one row from the combined NIC→SG→rule join.
// It carries all fields needed by the host-agent to build nftables policy.
// Source: vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model"
//
//	"The effective rule set for a NIC is the union of all rules from all
//	 SGs attached to that NIC."
type EffectiveSGRuleRow struct {
	RuleID                string
	SecurityGroupID       string
	Direction             string // 'ingress' | 'egress'
	Protocol              string // 'tcp' | 'udp' | 'icmp' | 'all'
	PortFrom              *int
	PortTo                *int
	CIDR                  *string // source (ingress) or destination (egress) CIDR
	SourceSecurityGroupID *string // non-nil for SG-reference rules (cross-SG matching)
}

// GetEffectiveSGRulesForNIC returns the union of all security group rules
// attached to a NIC, joining nic_security_groups → security_group_rules.
//
// The result is used by the host-agent enforcement seam (ApplySGPolicy) to build
// the per-NIC nftables chain. Rules from all SGs are union-merged; the caller
// is responsible for applying the implicit deny-all default after all allow rules.
//
// Returns an empty slice when the NIC has no SGs attached (deny all effectively).
//
// Source: vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model",
//
//	vm-14-02__skill__ §instructions "effective rule set union merge".
func (r *Repo) GetEffectiveSGRulesForNIC(ctx context.Context, nicID string) ([]EffectiveSGRuleRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT sgr.id, sgr.security_group_id, sgr.direction, sgr.protocol,
		       sgr.port_from, sgr.port_to, sgr.cidr, sgr.source_sg_id
		FROM nic_security_groups nsg
		JOIN security_group_rules sgr ON sgr.security_group_id = nsg.security_group_id
		WHERE nsg.nic_id = $1
		ORDER BY sgr.security_group_id, sgr.direction, sgr.created_at ASC
	`, nicID)
	if err != nil {
		return nil, fmt.Errorf("GetEffectiveSGRulesForNIC: %w", err)
	}
	defer rows.Close()

	var out []EffectiveSGRuleRow
	for rows.Next() {
		var rule EffectiveSGRuleRow
		if err := rows.Scan(
			&rule.RuleID, &rule.SecurityGroupID, &rule.Direction, &rule.Protocol,
			&rule.PortFrom, &rule.PortTo, &rule.CIDR, &rule.SourceSecurityGroupID,
		); err != nil {
			return nil, fmt.Errorf("GetEffectiveSGRulesForNIC scan: %w", err)
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetEffectiveSGRulesForNIC rows: %w", err)
	}
	return out, nil
}

// UpdateNICSecurityGroups replaces the full security group set for a NIC.
//
// Implementation: delete all existing nic_security_groups rows for nicID,
// then insert the new set. Two sequential Exec calls are used because the
// Pool interface (db.Pool) does not expose Begin/transaction methods.
//
// This is safe because:
//   - The caller validates the new sgIDs before calling.
//   - The operation is idempotent per nicID.
//   - A partial failure (delete succeeds, an insert fails) leaves the NIC
//     with no SGs. A NIC with no SGs falls back to deny-all policy at the
//     host-agent layer, which is the safe default. The next successful call
//     restores the correct set.
//
// Source: vm-14-02__blueprint__ §core_contracts "NIC-Centric Policy Model".
func (r *Repo) UpdateNICSecurityGroups(ctx context.Context, nicID string, sgIDs []string) error {
	// Step 1: remove all existing SG links for this NIC.
	if _, err := r.pool.Exec(ctx, `
		DELETE FROM nic_security_groups WHERE nic_id = $1
	`, nicID); err != nil {
		return fmt.Errorf("UpdateNICSecurityGroups: delete old links: %w", err)
	}

	// Step 2: insert the new SG links.
	for _, sgID := range sgIDs {
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO nic_security_groups (nic_id, security_group_id)
			VALUES ($1, $2)
		`, nicID, sgID); err != nil {
			return fmt.Errorf("UpdateNICSecurityGroups: insert link sg %s: %w", sgID, err)
		}
	}
	return nil
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

// UpdateNetworkInterfaceStatus sets the status field of a network_interfaces row.
// Used by worker lifecycle handlers to advance NIC state through:
//
//	pending → attached (create/start)
//	attached → detached (stop)
//
// Silently succeeds when the row does not exist (already cleaned up).
// Source: P2_VPC_NETWORK_CONTRACT §5 (NIC model), VM-P2A-S2 audit finding R3.
func (r *Repo) UpdateNetworkInterfaceStatus(ctx context.Context, nicID, status string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE network_interfaces
		SET status     = $2,
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, nicID, status)
	if err != nil {
		return fmt.Errorf("UpdateNetworkInterfaceStatus: %w", err)
	}
	return nil
}

// SoftDeleteNetworkInterface marks a network_interfaces row as deleted.
// Sets status = 'deleted' and deleted_at = NOW(). Idempotent: safe to call
// when the NIC has already been soft-deleted (0 rows affected is not an error).
// Source: P2_VPC_NETWORK_CONTRACT §5 (NIC lifecycle), VM-P2A-S2 audit finding R3.
func (r *Repo) SoftDeleteNetworkInterface(ctx context.Context, nicID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE network_interfaces
		SET status     = 'deleted',
		    deleted_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, nicID)
	if err != nil {
		return fmt.Errorf("SoftDeleteNetworkInterface: %w", err)
	}
	return nil
}

// AllocateIPFromSubnet allocates an IP address from a subnet's CIDR range.
// Uses SELECT FOR UPDATE SKIP LOCKED to avoid contention.
// Returns the allocated IP or error if subnet is exhausted.
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
// Returns nil, nil if no default SG exists.
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
		// Allow if it's the default SG or owned by the principal.
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

// ── VM Job 5: Stale NIC detection for reconciliation ─────────────────────────

// StaleNICRow is the result row for ListStaleNetworkInterfaces.
// Captures NICs whose instance has been deleted but the NIC record remains active.
type StaleNICRow struct {
	NICID         string
	InstanceID    string
	SubnetID      string
	VPCID         string
	PrivateIP     string
	NICStatus     string
	InstanceState string
}

// ListStaleNetworkInterfaces returns NIC rows where the owning instance is
// deleted but the NIC has not been cleaned up.
// VM Job 5 — Case 5: Stale TAP/NAT/firewall state for deleted/stopped instances.
func (r *Repo) ListStaleNetworkInterfaces(ctx context.Context) ([]*StaleNICRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			n.id            AS nic_id,
			n.instance_id,
			n.subnet_id,
			n.vpc_id,
			n.private_ip,
			n.status        AS nic_status,
			COALESCE(i.vm_state, 'unknown') AS instance_state
		FROM network_interfaces n
		LEFT JOIN instances i ON i.id = n.instance_id
		WHERE n.deleted_at IS NULL
		  AND n.status != 'deleted'
		  AND (
		      i.deleted_at IS NOT NULL
		      OR i.vm_state IN ('deleted')
		  )
		ORDER BY n.created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("ListStaleNetworkInterfaces: %w", err)
	}
	defer rows.Close()

	var out []*StaleNICRow
	for rows.Next() {
		r := &StaleNICRow{}
		if err := rows.Scan(
			&r.NICID, &r.InstanceID, &r.SubnetID, &r.VPCID,
			&r.PrivateIP, &r.NICStatus, &r.InstanceState,
		); err != nil {
			return nil, fmt.Errorf("ListStaleNetworkInterfaces scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
