package domainmodel

import "time"

// NICVTEPRegistrationStatus represents the status of a NIC's VTEP registration.
type NICVTEPRegistrationStatus string

const (
	NICVTEPRegistrationStatusPending NICVTEPRegistrationStatus = "pending"
	NICVTEPRegistrationStatusActive  NICVTEPRegistrationStatus = "active"
	NICVTEPRegistrationStatusStale   NICVTEPRegistrationStatus = "stale"
	NICVTEPRegistrationStatusRemoved NICVTEPRegistrationStatus = "removed"
)

// NICVTEPRegistration tracks a NIC's registration with the network controller.
// When a VPC instance is created, the host agent registers the NIC's MAC and IP.
// This enables the network controller to build the forwarding table:
//
//	(vpc_id, instance_private_ip) → host_VTEP_IP
//
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (host agent registers NIC MAC/IP).
type NICVTEPRegistration struct {
	ID           string                    `json:"id" db:"id"`
	NICID        string                    `json:"nic_id" db:"nic_id"`
	VPCID        string                    `json:"vpc_id" db:"vpc_id"`
	HostID       string                    `json:"host_id" db:"host_id"`
	PrivateIP    string                    `json:"private_ip" db:"private_ip"`
	MACAddress   string                    `json:"mac_address" db:"mac_address"`
	VNI          int                       `json:"vni" db:"vni"`
	Status       NICVTEPRegistrationStatus `json:"status" db:"status"`
	RegisteredAt time.Time                 `json:"registered_at" db:"registered_at"`
	UpdatedAt    time.Time                 `json:"updated_at" db:"updated_at"`
}
