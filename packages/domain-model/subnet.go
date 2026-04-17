package domainmodel

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
	AvailabilityZone string       `json:"availability_zone" db:"availability_zone"`
	Status           SubnetStatus `json:"status" db:"status"`
	CreatedAt        time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at" db:"updated_at"`
	DeletedAt        *time.Time   `json:"deleted_at,omitempty" db:"deleted_at"`
}
