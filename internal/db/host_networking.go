package db

// host_networking.go — Cross-host networking groundwork persistence methods.
//
// M9 Slice 4: Control-plane foundation for future VXLAN overlay networking.
//
// Source: P2_VPC_NETWORK_CONTRACT §8 (VXLAN Implementation Model),
//         PHASE_2_MASTER_PLAN §8 (networking subsystem evolution),
//         P2_MILESTONE_PLAN §P2-M2, §P2-M3.
//
// This file provides repo methods for:
//   - host_tunnel_endpoints: per-host VTEP identity
//   - vpc_host_attachments: host participation in VPCs
//   - vni_allocations: VXLAN VNI pool management
//   - nic_vtep_registrations: NIC → host VTEP forwarding entries

import (
	"context"
	"fmt"
	"time"
)

// ── HostTunnelEndpoint ───────────────────────────────────────────────────────

// HostTunnelEndpointRow is the DB representation of a host's VTEP record.
// Source: P2_VPC_NETWORK_CONTRACT §8.2.
type HostTunnelEndpointRow struct {
	HostID        string
	VTEPIP        string
	VTEPMAC       *string
	VTEPInterface string
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// UpsertHostTunnelEndpoint creates or updates a host's VTEP record.
// Called during host registration when the host agent reports its VTEP IP.
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (each compute host has one VTEP interface).
func (r *Repo) UpsertHostTunnelEndpoint(ctx context.Context, row *HostTunnelEndpointRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO host_tunnel_endpoints (
			host_id, vtep_ip, vtep_mac, vtep_interface, status, created_at, updated_at
		) VALUES ($1, $2::INET, $3::MACADDR, $4, $5, NOW(), NOW())
		ON CONFLICT (host_id) DO UPDATE
		SET vtep_ip = EXCLUDED.vtep_ip,
		    vtep_mac = EXCLUDED.vtep_mac,
		    vtep_interface = EXCLUDED.vtep_interface,
		    status = EXCLUDED.status,
		    updated_at = NOW()
	`,
		row.HostID, row.VTEPIP, row.VTEPMAC, row.VTEPInterface, row.Status,
	)
	if err != nil {
		return fmt.Errorf("UpsertHostTunnelEndpoint: %w", err)
	}
	return nil
}

// GetHostTunnelEndpoint fetches a host's VTEP record by host_id.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetHostTunnelEndpoint(ctx context.Context, hostID string) (*HostTunnelEndpointRow, error) {
	row := &HostTunnelEndpointRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT host_id, vtep_ip::TEXT, vtep_mac::TEXT, vtep_interface, status, created_at, updated_at
		FROM host_tunnel_endpoints
		WHERE host_id = $1
	`, hostID).Scan(
		&row.HostID, &row.VTEPIP, &row.VTEPMAC, &row.VTEPInterface,
		&row.Status, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetHostTunnelEndpoint: %w", err)
	}
	return row, nil
}

// ListActiveHostTunnelEndpoints returns all active host VTEPs.
// Used by the network controller to build the global VTEP table.
func (r *Repo) ListActiveHostTunnelEndpoints(ctx context.Context) ([]*HostTunnelEndpointRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT host_id, vtep_ip::TEXT, vtep_mac::TEXT, vtep_interface, status, created_at, updated_at
		FROM host_tunnel_endpoints
		WHERE status = 'active'
		ORDER BY host_id
	`)
	if err != nil {
		return nil, fmt.Errorf("ListActiveHostTunnelEndpoints: %w", err)
	}
	defer rows.Close()

	var out []*HostTunnelEndpointRow
	for rows.Next() {
		row := &HostTunnelEndpointRow{}
		if err := rows.Scan(
			&row.HostID, &row.VTEPIP, &row.VTEPMAC, &row.VTEPInterface,
			&row.Status, &row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListActiveHostTunnelEndpoints scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateHostTunnelEndpointStatus updates a host VTEP's status (e.g., for draining).
// Returns error if host not found (0 rows affected).
func (r *Repo) UpdateHostTunnelEndpointStatus(ctx context.Context, hostID, status string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE host_tunnel_endpoints
		SET status = $2,
		    updated_at = NOW()
		WHERE host_id = $1
	`, hostID, status)
	if err != nil {
		return fmt.Errorf("UpdateHostTunnelEndpointStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateHostTunnelEndpointStatus: host %s not found", hostID)
	}
	return nil
}

// ── VPCHostAttachment ────────────────────────────────────────────────────────

// VPCHostAttachmentRow is the DB representation of a VPC-host participation record.
// Source: P2_VPC_NETWORK_CONTRACT §8.2.
type VPCHostAttachmentRow struct {
	ID              string
	VPCID           string
	HostID          string
	InstanceCount   int
	FirstAttachedAt time.Time
	LastUpdatedAt   time.Time
}

// IncrementVPCHostAttachment increments the instance count for a VPC-host pair.
// If no record exists, creates one with instance_count=1.
// Called when a VPC instance is created on a host.
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (network controller propagates VTEP entries).
func (r *Repo) IncrementVPCHostAttachment(ctx context.Context, id, vpcID, hostID string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO vpc_host_attachments (
			id, vpc_id, host_id, instance_count, first_attached_at, last_updated_at
		) VALUES ($1, $2, $3, 1, NOW(), NOW())
		ON CONFLICT (vpc_id, host_id) DO UPDATE
		SET instance_count = vpc_host_attachments.instance_count + 1,
		    last_updated_at = NOW()
	`, id, vpcID, hostID)
	if err != nil {
		return fmt.Errorf("IncrementVPCHostAttachment: %w", err)
	}
	return nil
}

// DecrementVPCHostAttachment decrements the instance count for a VPC-host pair.
// Called when a VPC instance is deleted from a host.
// Does not error if count would go negative (sets to 0).
func (r *Repo) DecrementVPCHostAttachment(ctx context.Context, vpcID, hostID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE vpc_host_attachments
		SET instance_count = GREATEST(0, instance_count - 1),
		    last_updated_at = NOW()
		WHERE vpc_id = $1
		  AND host_id = $2
	`, vpcID, hostID)
	if err != nil {
		return fmt.Errorf("DecrementVPCHostAttachment: %w", err)
	}
	return nil
}

// GetVPCHostAttachment fetches a VPC-host attachment by VPC and host IDs.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetVPCHostAttachment(ctx context.Context, vpcID, hostID string) (*VPCHostAttachmentRow, error) {
	row := &VPCHostAttachmentRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, vpc_id, host_id, instance_count, first_attached_at, last_updated_at
		FROM vpc_host_attachments
		WHERE vpc_id = $1
		  AND host_id = $2
	`, vpcID, hostID).Scan(
		&row.ID, &row.VPCID, &row.HostID, &row.InstanceCount,
		&row.FirstAttachedAt, &row.LastUpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetVPCHostAttachment: %w", err)
	}
	return row, nil
}

// ListHostsInVPC returns all hosts with active instances in a VPC.
// Used by the network controller to determine which hosts need VTEP entries.
func (r *Repo) ListHostsInVPC(ctx context.Context, vpcID string) ([]*VPCHostAttachmentRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, vpc_id, host_id, instance_count, first_attached_at, last_updated_at
		FROM vpc_host_attachments
		WHERE vpc_id = $1
		  AND instance_count > 0
		ORDER BY first_attached_at ASC
	`, vpcID)
	if err != nil {
		return nil, fmt.Errorf("ListHostsInVPC: %w", err)
	}
	defer rows.Close()

	var out []*VPCHostAttachmentRow
	for rows.Next() {
		row := &VPCHostAttachmentRow{}
		if err := rows.Scan(
			&row.ID, &row.VPCID, &row.HostID, &row.InstanceCount,
			&row.FirstAttachedAt, &row.LastUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListHostsInVPC scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListVPCsOnHost returns all VPCs with active instances on a host.
// Used for host draining to determine which VPCs need VTEP cleanup.
func (r *Repo) ListVPCsOnHost(ctx context.Context, hostID string) ([]*VPCHostAttachmentRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, vpc_id, host_id, instance_count, first_attached_at, last_updated_at
		FROM vpc_host_attachments
		WHERE host_id = $1
		  AND instance_count > 0
		ORDER BY first_attached_at ASC
	`, hostID)
	if err != nil {
		return nil, fmt.Errorf("ListVPCsOnHost: %w", err)
	}
	defer rows.Close()

	var out []*VPCHostAttachmentRow
	for rows.Next() {
		row := &VPCHostAttachmentRow{}
		if err := rows.Scan(
			&row.ID, &row.VPCID, &row.HostID, &row.InstanceCount,
			&row.FirstAttachedAt, &row.LastUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListVPCsOnHost scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── VNI Allocation ───────────────────────────────────────────────────────────

// AllocateVNI atomically allocates a VNI for a VPC.
// Uses SELECT FOR UPDATE SKIP LOCKED to avoid contention.
// Returns the allocated VNI or error if pool is exhausted.
// Source: P2_VPC_NETWORK_CONTRACT §8.1 (each VPC is assigned a unique VNI).
func (r *Repo) AllocateVNI(ctx context.Context, vpcID string) (int, error) {
	var vni int
	err := r.pool.QueryRow(ctx, `
		UPDATE vni_allocations
		SET allocated = TRUE,
		    vpc_id = $1,
		    allocated_at = NOW()
		WHERE vni = (
			SELECT vni
			FROM vni_allocations
			WHERE allocated = FALSE
			ORDER BY vni
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING vni
	`, vpcID).Scan(&vni)
	if err != nil {
		if isNoRowsErr(err) {
			return 0, fmt.Errorf("AllocateVNI: no available VNIs")
		}
		return 0, fmt.Errorf("AllocateVNI: %w", err)
	}
	return vni, nil
}

// ReleaseVNI releases a VNI back to the pool.
// Called when a VPC is deleted.
// Idempotent: does not error if VNI was not allocated.
func (r *Repo) ReleaseVNI(ctx context.Context, vni int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE vni_allocations
		SET allocated = FALSE,
		    vpc_id = NULL,
		    allocated_at = NULL
		WHERE vni = $1
	`, vni)
	if err != nil {
		return fmt.Errorf("ReleaseVNI: %w", err)
	}
	return nil
}

// GetVNIByVPC returns the VNI allocated to a VPC.
// Returns 0, nil if no VNI is allocated (VPC not yet provisioned).
func (r *Repo) GetVNIByVPC(ctx context.Context, vpcID string) (int, error) {
	var vni int
	err := r.pool.QueryRow(ctx, `
		SELECT vni
		FROM vni_allocations
		WHERE vpc_id = $1
		  AND allocated = TRUE
	`, vpcID).Scan(&vni)
	if err != nil {
		if isNoRowsErr(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetVNIByVPC: %w", err)
	}
	return vni, nil
}

// ── NIC VTEP Registration ────────────────────────────────────────────────────

// NICVTEPRegistrationRow is the DB representation of a NIC's VTEP registration.
// Source: P2_VPC_NETWORK_CONTRACT §8.2.
type NICVTEPRegistrationRow struct {
	ID           string
	NICID        string
	VPCID        string
	HostID       string
	PrivateIP    string
	MACAddress   string
	VNI          int
	Status       string
	RegisteredAt time.Time
	UpdatedAt    time.Time
}

// CreateNICVTEPRegistration creates a NIC VTEP registration record.
// Called when a VPC instance is created and the host agent reports the NIC.
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (host agent registers NIC MAC/IP).
func (r *Repo) CreateNICVTEPRegistration(ctx context.Context, row *NICVTEPRegistrationRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO nic_vtep_registrations (
			id, nic_id, vpc_id, host_id, private_ip, mac_address, vni, status, registered_at, updated_at
		) VALUES ($1, $2, $3, $4, $5::INET, $6::MACADDR, $7, $8, NOW(), NOW())
	`,
		row.ID, row.NICID, row.VPCID, row.HostID, row.PrivateIP,
		row.MACAddress, row.VNI, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateNICVTEPRegistration: %w", err)
	}
	return nil
}

// GetNICVTEPRegistration fetches a NIC VTEP registration by ID.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetNICVTEPRegistration(ctx context.Context, id string) (*NICVTEPRegistrationRow, error) {
	row := &NICVTEPRegistrationRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, nic_id, vpc_id, host_id, private_ip::TEXT, mac_address::TEXT, vni, status, registered_at, updated_at
		FROM nic_vtep_registrations
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.NICID, &row.VPCID, &row.HostID, &row.PrivateIP,
		&row.MACAddress, &row.VNI, &row.Status, &row.RegisteredAt, &row.UpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetNICVTEPRegistration: %w", err)
	}
	return row, nil
}

// GetNICVTEPRegistrationByNIC fetches a NIC VTEP registration by NIC ID.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetNICVTEPRegistrationByNIC(ctx context.Context, nicID string) (*NICVTEPRegistrationRow, error) {
	row := &NICVTEPRegistrationRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, nic_id, vpc_id, host_id, private_ip::TEXT, mac_address::TEXT, vni, status, registered_at, updated_at
		FROM nic_vtep_registrations
		WHERE nic_id = $1
		  AND status = 'active'
	`, nicID).Scan(
		&row.ID, &row.NICID, &row.VPCID, &row.HostID, &row.PrivateIP,
		&row.MACAddress, &row.VNI, &row.Status, &row.RegisteredAt, &row.UpdatedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetNICVTEPRegistrationByNIC: %w", err)
	}
	return row, nil
}

// ListNICVTEPRegistrationsByVPC returns all active NIC VTEP registrations in a VPC.
// Used by the network controller to build the forwarding table for a VPC.
func (r *Repo) ListNICVTEPRegistrationsByVPC(ctx context.Context, vpcID string) ([]*NICVTEPRegistrationRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, nic_id, vpc_id, host_id, private_ip::TEXT, mac_address::TEXT, vni, status, registered_at, updated_at
		FROM nic_vtep_registrations
		WHERE vpc_id = $1
		  AND status = 'active'
		ORDER BY registered_at ASC
	`, vpcID)
	if err != nil {
		return nil, fmt.Errorf("ListNICVTEPRegistrationsByVPC: %w", err)
	}
	defer rows.Close()

	var out []*NICVTEPRegistrationRow
	for rows.Next() {
		row := &NICVTEPRegistrationRow{}
		if err := rows.Scan(
			&row.ID, &row.NICID, &row.VPCID, &row.HostID, &row.PrivateIP,
			&row.MACAddress, &row.VNI, &row.Status, &row.RegisteredAt, &row.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListNICVTEPRegistrationsByVPC scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListNICVTEPRegistrationsByHost returns all active NIC VTEP registrations on a host.
// Used for host draining to identify all VPC NICs that need cleanup.
func (r *Repo) ListNICVTEPRegistrationsByHost(ctx context.Context, hostID string) ([]*NICVTEPRegistrationRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, nic_id, vpc_id, host_id, private_ip::TEXT, mac_address::TEXT, vni, status, registered_at, updated_at
		FROM nic_vtep_registrations
		WHERE host_id = $1
		  AND status = 'active'
		ORDER BY registered_at ASC
	`, hostID)
	if err != nil {
		return nil, fmt.Errorf("ListNICVTEPRegistrationsByHost: %w", err)
	}
	defer rows.Close()

	var out []*NICVTEPRegistrationRow
	for rows.Next() {
		row := &NICVTEPRegistrationRow{}
		if err := rows.Scan(
			&row.ID, &row.NICID, &row.VPCID, &row.HostID, &row.PrivateIP,
			&row.MACAddress, &row.VNI, &row.Status, &row.RegisteredAt, &row.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListNICVTEPRegistrationsByHost scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateNICVTEPRegistrationStatus updates a NIC VTEP registration's status.
// Used when a NIC is deleted (status='removed') or becomes stale.
func (r *Repo) UpdateNICVTEPRegistrationStatus(ctx context.Context, id, status string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE nic_vtep_registrations
		SET status = $2,
		    updated_at = NOW()
		WHERE id = $1
	`, id, status)
	if err != nil {
		return fmt.Errorf("UpdateNICVTEPRegistrationStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateNICVTEPRegistrationStatus: registration %s not found", id)
	}
	return nil
}

// LookupVTEPForPrivateIP looks up the host VTEP IP for a private IP within a VPC.
// This is the core forwarding table lookup for cross-host traffic.
// Returns the host's VTEP IP or error if not found.
func (r *Repo) LookupVTEPForPrivateIP(ctx context.Context, vpcID, privateIP string) (string, error) {
	var vtepIP string
	err := r.pool.QueryRow(ctx, `
		SELECT hte.vtep_ip::TEXT
		FROM nic_vtep_registrations nvr
		JOIN host_tunnel_endpoints hte ON nvr.host_id = hte.host_id
		WHERE nvr.vpc_id = $1
		  AND nvr.private_ip = $2::INET
		  AND nvr.status = 'active'
		  AND hte.status = 'active'
	`, vpcID, privateIP).Scan(&vtepIP)
	if err != nil {
		if isNoRowsErr(err) {
			return "", fmt.Errorf("LookupVTEPForPrivateIP: no VTEP for %s in vpc %s", privateIP, vpcID)
		}
		return "", fmt.Errorf("LookupVTEPForPrivateIP: %w", err)
	}
	return vtepIP, nil
}
