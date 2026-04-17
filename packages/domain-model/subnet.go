package domainmodel

// subnet.go — Subnet domain type.
//
// VM-P3A Job 1: Added CIDRIPv6 to support dual-stack subnets.
// Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate"
//   "A Subnet must have an IPv4 CIDR and a /64 IPv6 block."
// CIDRIPv6 is nullable: nil for Phase 2 IPv4-only subnets; set for P3A dual-stack subnets.

import "time"

type SubnetStatus string

const (
	SubnetStatusActive   SubnetStatus = "active"
	SubnetStatusDeleting SubnetStatus = "deleting"
	SubnetStatusDeleted  SubnetStatus = "deleted"
	SubnetStatusFailed   SubnetStatus = "failed"
)

type Subnet struct {
	ID               string       `json:"id" db:"id"`
	VPCID            string       `json:"vpc_id" db:"vpc_id"`
	Name             string       `json:"name" db:"name"`
	CIDRIPv4         string       `json:"cidr_ipv4" db:"cidr_ipv4"`
	CIDRIPv6         *string      `json:"cidr_ipv6,omitempty" db:"cidr_ipv6"`
	AvailabilityZone string       `json:"availability_zone" db:"availability_zone"`
	Status           SubnetStatus `json:"status" db:"status"`
	CreatedAt        time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at" db:"updated_at"`
	DeletedAt        *time.Time   `json:"deleted_at,omitempty" db:"deleted_at"`
}
