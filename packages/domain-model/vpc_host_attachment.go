package domainmodel

import "time"

// VPCHostAttachment tracks which hosts participate in which VPCs.
// When an instance is created in a VPC on a host, an attachment record is
// created (or the instance_count is incremented). The network controller
// uses this to know which hosts need VTEP entries for a given VPC.
//
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (network controller propagates VTEP
//         entries to all other hosts in the VPC).
type VPCHostAttachment struct {
	ID              string    `json:"id" db:"id"`
	VPCID           string    `json:"vpc_id" db:"vpc_id"`
	HostID          string    `json:"host_id" db:"host_id"`
	InstanceCount   int       `json:"instance_count" db:"instance_count"`
	FirstAttachedAt time.Time `json:"first_attached_at" db:"first_attached_at"`
	LastUpdatedAt   time.Time `json:"last_updated_at" db:"last_updated_at"`
}
