package db

// eip_repo.go — Elastic IP and NAT Gateway persistence methods.
//
// VM-P3A Job 2: Public connectivity maturity.
//
// Elastic IP model:
//   - EIPs are owner-scoped public IPv4 addresses.
//   - An EIP can be unassociated ('none'), associated to a NIC ('nic'),
//     or associated to a NAT Gateway ('nat_gateway').
//   - Association and disassociation are explicit operations that update
//     association_type and associated_resource_id atomically.
//   - One public IP is unique across all non-deleted EIPs (enforced by DB index).
//
// NAT Gateway model:
//   - NAT Gateways are subnet-scoped (one per subnet, enforced by DB index).
//   - Each NAT Gateway holds one EIP for outbound SNAT.
//   - Status lifecycle: pending → available → deleting → deleted.
//
// Ownership:
//   - All lookups that return data to API callers must filter by owner_principal_id.
//   - Cross-owner access returns nil, nil (not found), never an error.
//   - 404-for-cross-account is enforced at the handler level using these nil returns.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Allocation and Association",
//         "NAT Gateway Lifecycle", "Public Connectivity Contract".

import (
	"context"
	"fmt"
	"time"
)

// ── ElasticIP ─────────────────────────────────────────────────────────────────

// ElasticIPRow is the DB representation of an elastic_ips record.
type ElasticIPRow struct {
	ID                   string
	OwnerPrincipalID     string
	PublicIP             string
	AssociationType      string  // 'none' | 'nic' | 'nat_gateway'
	AssociatedResourceID *string // nil when association_type = 'none'
	Status               string  // 'available' | 'associated' | 'releasing' | 'released'
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            *time.Time
}

// EIPAlreadyAssociatedError is returned when an EIP is already associated to
// a resource and a second association is attempted.
type EIPAlreadyAssociatedError struct {
	EIPID               string
	ExistingAssociation string // current associated_resource_id
}

func (e *EIPAlreadyAssociatedError) Error() string {
	return fmt.Sprintf("elastic ip %s is already associated to resource %s", e.EIPID, e.ExistingAssociation)
}

// CreateElasticIP inserts a new Elastic IP record.
// The public_ip is provided by the platform's public IP pool; this method
// only persists the record — it does not allocate from a network pool.
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Allocation".
func (r *Repo) CreateElasticIP(ctx context.Context, row *ElasticIPRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO elastic_ips (
			id, owner_principal_id, public_ip, association_type,
			associated_resource_id, status, created_at, updated_at
		) VALUES ($1, $2, $3::INET, $4, $5, $6, NOW(), NOW())
	`,
		row.ID, row.OwnerPrincipalID, row.PublicIP, row.AssociationType,
		row.AssociatedResourceID, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateElasticIP: %w", err)
	}
	return nil
}

// GetElasticIPByID fetches a single Elastic IP by its primary key.
// Returns nil, nil when no matching row exists (including deleted).
func (r *Repo) GetElasticIPByID(ctx context.Context, id string) (*ElasticIPRow, error) {
	row := &ElasticIPRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_principal_id, public_ip::TEXT, association_type,
		       associated_resource_id, status, created_at, updated_at, deleted_at
		FROM elastic_ips
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.OwnerPrincipalID, &row.PublicIP, &row.AssociationType,
		&row.AssociatedResourceID, &row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetElasticIPByID: %w", err)
	}
	return row, nil
}

// ListElasticIPsByOwner returns all non-deleted Elastic IPs for an owner principal.
func (r *Repo) ListElasticIPsByOwner(ctx context.Context, ownerPrincipalID string) ([]*ElasticIPRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_principal_id, public_ip::TEXT, association_type,
		       associated_resource_id, status, created_at, updated_at, deleted_at
		FROM elastic_ips
		WHERE owner_principal_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`, ownerPrincipalID)
	if err != nil {
		return nil, fmt.Errorf("ListElasticIPsByOwner: %w", err)
	}
	defer rows.Close()

	var out []*ElasticIPRow
	for rows.Next() {
		row := &ElasticIPRow{}
		if err := rows.Scan(
			&row.ID, &row.OwnerPrincipalID, &row.PublicIP, &row.AssociationType,
			&row.AssociatedResourceID, &row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListElasticIPsByOwner scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AssociateElasticIP atomically associates an EIP with a resource (NIC or NAT GW).
// Returns *EIPAlreadyAssociatedError if the EIP is not in 'none' state.
// The caller is responsible for verifying the target resource exists and is owned.
//
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Association":
//   "An EIP can only be associated to one resource at a time."
func (r *Repo) AssociateElasticIP(ctx context.Context, eipID, resourceID, associationType string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE elastic_ips
		SET association_type        = $2,
		    associated_resource_id  = $3,
		    status                  = 'associated',
		    updated_at              = NOW()
		WHERE id = $1
		  AND association_type = 'none'
		  AND deleted_at IS NULL
	`, eipID, associationType, resourceID)
	if err != nil {
		return fmt.Errorf("AssociateElasticIP: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Could be already associated or not found — fetch to distinguish.
		existing, fetchErr := r.GetElasticIPByID(ctx, eipID)
		if fetchErr != nil {
			return fmt.Errorf("AssociateElasticIP: check failed: %w", fetchErr)
		}
		if existing == nil || existing.DeletedAt != nil {
			return fmt.Errorf("AssociateElasticIP: eip %s not found", eipID)
		}
		// EIP exists but is already associated.
		return &EIPAlreadyAssociatedError{
			EIPID:               eipID,
			ExistingAssociation: func() string {
				if existing.AssociatedResourceID != nil {
					return *existing.AssociatedResourceID
				}
				return "(unknown)"
			}(),
		}
	}
	return nil
}

// DisassociateElasticIP atomically removes an EIP's association.
// Safe to call when the EIP is already unassociated (idempotent — 0 rows affected is OK).
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Disassociation".
func (r *Repo) DisassociateElasticIP(ctx context.Context, eipID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE elastic_ips
		SET association_type       = 'none',
		    associated_resource_id = NULL,
		    status                 = 'available',
		    updated_at             = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, eipID)
	if err != nil {
		return fmt.Errorf("DisassociateElasticIP: %w", err)
	}
	// 0 rows = EIP deleted or not found; callers verify ownership before calling.
	return nil
}

// SoftDeleteElasticIP marks an EIP as released.
// An EIP can only be deleted when it is unassociated (association_type='none').
// Returns error if EIP is still associated (0 rows affected with that filter).
func (r *Repo) SoftDeleteElasticIP(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE elastic_ips
		SET deleted_at       = NOW(),
		    updated_at       = NOW(),
		    status           = 'released'
		WHERE id = $1
		  AND association_type = 'none'
		  AND deleted_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("SoftDeleteElasticIP: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either not found, already deleted, or still associated.
		existing, _ := r.GetElasticIPByID(ctx, id)
		if existing != nil && existing.AssociationType != "none" {
			return fmt.Errorf("SoftDeleteElasticIP: eip %s is still associated; disassociate first", id)
		}
		return fmt.Errorf("SoftDeleteElasticIP: eip %s not found or already released", id)
	}
	return nil
}

// GetElasticIPByAssociatedResource finds the EIP associated to a given resource ID.
// Returns nil, nil when no EIP is currently associated to that resource.
// Used to find and disassociate the EIP when a NIC or NAT Gateway is deleted.
func (r *Repo) GetElasticIPByAssociatedResource(ctx context.Context, resourceID string) (*ElasticIPRow, error) {
	row := &ElasticIPRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_principal_id, public_ip::TEXT, association_type,
		       associated_resource_id, status, created_at, updated_at, deleted_at
		FROM elastic_ips
		WHERE associated_resource_id = $1
		  AND association_type != 'none'
		  AND deleted_at IS NULL
	`, resourceID).Scan(
		&row.ID, &row.OwnerPrincipalID, &row.PublicIP, &row.AssociationType,
		&row.AssociatedResourceID, &row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetElasticIPByAssociatedResource: %w", err)
	}
	return row, nil
}

// ── NATGateway ────────────────────────────────────────────────────────────────

// NATGatewayRow is the DB representation of a nat_gateways record.
type NATGatewayRow struct {
	ID               string
	OwnerPrincipalID string
	VPCID            string
	SubnetID         string
	ElasticIPID      string
	Status           string // 'pending' | 'available' | 'deleting' | 'deleted' | 'failed'
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// NATGatewaySubnetConflictError is returned when a subnet already has an active NAT Gateway.
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Anti-Loop"
//   (one NAT Gateway per subnet prevents routing confusion).
type NATGatewaySubnetConflictError struct {
	SubnetID         string
	ExistingNATGWID  string
}

func (e *NATGatewaySubnetConflictError) Error() string {
	return fmt.Sprintf("subnet %s already has a nat gateway (%s)", e.SubnetID, e.ExistingNATGWID)
}

// CreateNATGateway inserts a new NAT Gateway record.
// The unique partial index on subnet_id enforces one-per-subnet at the DB level.
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Lifecycle".
func (r *Repo) CreateNATGateway(ctx context.Context, row *NATGatewayRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO nat_gateways (
			id, owner_principal_id, vpc_id, subnet_id, elastic_ip_id, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
	`,
		row.ID, row.OwnerPrincipalID, row.VPCID, row.SubnetID, row.ElasticIPID, row.Status,
	)
	if err != nil {
		return fmt.Errorf("CreateNATGateway: %w", err)
	}
	return nil
}

// GetNATGatewayByID fetches a single NAT Gateway by its primary key.
// Returns nil, nil when no matching row exists.
func (r *Repo) GetNATGatewayByID(ctx context.Context, id string) (*NATGatewayRow, error) {
	row := &NATGatewayRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_principal_id, vpc_id, subnet_id, elastic_ip_id, status,
		       created_at, updated_at, deleted_at
		FROM nat_gateways
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.OwnerPrincipalID, &row.VPCID, &row.SubnetID, &row.ElasticIPID,
		&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetNATGatewayByID: %w", err)
	}
	return row, nil
}

// GetNATGatewayBySubnet returns the active NAT Gateway for a subnet, if any.
// Returns nil, nil when no active NAT Gateway exists in that subnet.
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Anti-Loop".
func (r *Repo) GetNATGatewayBySubnet(ctx context.Context, subnetID string) (*NATGatewayRow, error) {
	row := &NATGatewayRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, owner_principal_id, vpc_id, subnet_id, elastic_ip_id, status,
		       created_at, updated_at, deleted_at
		FROM nat_gateways
		WHERE subnet_id = $1
		  AND deleted_at IS NULL
		LIMIT 1
	`, subnetID).Scan(
		&row.ID, &row.OwnerPrincipalID, &row.VPCID, &row.SubnetID, &row.ElasticIPID,
		&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
	)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetNATGatewayBySubnet: %w", err)
	}
	return row, nil
}

// ListNATGatewaysByVPC returns all non-deleted NAT Gateways in a VPC.
func (r *Repo) ListNATGatewaysByVPC(ctx context.Context, vpcID string) ([]*NATGatewayRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, owner_principal_id, vpc_id, subnet_id, elastic_ip_id, status,
		       created_at, updated_at, deleted_at
		FROM nat_gateways
		WHERE vpc_id = $1
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`, vpcID)
	if err != nil {
		return nil, fmt.Errorf("ListNATGatewaysByVPC: %w", err)
	}
	defer rows.Close()

	var out []*NATGatewayRow
	for rows.Next() {
		row := &NATGatewayRow{}
		if err := rows.Scan(
			&row.ID, &row.OwnerPrincipalID, &row.VPCID, &row.SubnetID, &row.ElasticIPID,
			&row.Status, &row.CreatedAt, &row.UpdatedAt, &row.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("ListNATGatewaysByVPC scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateNATGatewayStatus advances a NAT Gateway's status.
// Returns error if the gateway is not found (0 rows affected).
func (r *Repo) UpdateNATGatewayStatus(ctx context.Context, id, status string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE nat_gateways
		SET status     = $2,
		    updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id, status)
	if err != nil {
		return fmt.Errorf("UpdateNATGatewayStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("UpdateNATGatewayStatus: nat gateway %s not found", id)
	}
	return nil
}

// SoftDeleteNATGateway marks a NAT Gateway as deleted.
// Does NOT automatically disassociate the EIP — callers must handle EIP disassociation
// before or after calling this, depending on their cleanup order.
// Returns error if gateway not found or already deleted (0 rows affected).
func (r *Repo) SoftDeleteNATGateway(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE nat_gateways
		SET deleted_at = NOW(),
		    updated_at = NOW(),
		    status     = 'deleted'
		WHERE id = $1
		  AND deleted_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("SoftDeleteNATGateway: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("SoftDeleteNATGateway: nat gateway %s not found or already deleted", id)
	}
	return nil
}
