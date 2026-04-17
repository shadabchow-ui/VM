package domainmodel

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
	Status           VPCStatus  `json:"status" db:"status"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}
