package domainmodel

// vpc.go — VPC domain type.
//
// VM-P3A Job 1: Added CIDRIPv6 to support dual-stack VPCs.
// Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate"
//   "A VPC must have both an IPv4 CIDR and a /56 IPv6 GUA block."
// CIDRIPv6 is nullable: nil for Phase 2 IPv4-only VPCs; set for P3A dual-stack VPCs.

import "time"

type VPCStatus string

const (
	VPCStatusActive   VPCStatus = "active"
	VPCStatusDeleting VPCStatus = "deleting"
	VPCStatusDeleted  VPCStatus = "deleted"
	VPCStatusFailed   VPCStatus = "failed"
)

type VPC struct {
	ID               string     `json:"id" db:"id"`
	OwnerPrincipalID string     `json:"owner_principal_id" db:"owner_principal_id"`
	Name             string     `json:"name" db:"name"`
	CIDRIPv4         string     `json:"cidr_ipv4" db:"cidr_ipv4"`
	CIDRIPv6         *string    `json:"cidr_ipv6,omitempty" db:"cidr_ipv6"`
	Status           VPCStatus  `json:"status" db:"status"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}
