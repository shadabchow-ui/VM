package domainmodel

// network_interface.go — NIC domain type.
//
// VM-P3A Job 1: Added PrivateIPv6 for dual-stack NIC addressing.
// DeviceIndex was already in the DB schema (device_index column) but missing
// from the domain type; added now for completeness.
//
// Source: vm-14-01__blueprint__ §core_contracts "Dual-Stack Mandate"
//   "A NIC must be assigned both an IPv4 and an IPv6 address."
// PrivateIPv6 is nullable: nil for Phase 2 IPv4-only NICs; set by the worker
// during dual-stack subnet allocation.

import "time"

type NetworkInterfaceStatus string

const (
	NetworkInterfaceStatusAttaching NetworkInterfaceStatus = "attaching"
	NetworkInterfaceStatusAttached  NetworkInterfaceStatus = "attached"
	NetworkInterfaceStatusDetaching NetworkInterfaceStatus = "detaching"
	NetworkInterfaceStatusDetached  NetworkInterfaceStatus = "detached"
	NetworkInterfaceStatusPending   NetworkInterfaceStatus = "pending"
	NetworkInterfaceStatusFailed    NetworkInterfaceStatus = "failed"
)

type NetworkInterface struct {
	ID          string                 `json:"id" db:"id"`
	InstanceID  string                 `json:"instance_id" db:"instance_id"`
	VPCID       string                 `json:"vpc_id" db:"vpc_id"`
	SubnetID    string                 `json:"subnet_id" db:"subnet_id"`
	PrivateIP   string                 `json:"private_ip" db:"private_ip"`
	PrivateIPv6 *string                `json:"private_ipv6,omitempty" db:"private_ipv6"`
	MACAddress  string                 `json:"mac_address" db:"mac_address"`
	DeviceIndex int                    `json:"device_index" db:"device_index"`
	Status      NetworkInterfaceStatus `json:"status" db:"status"`
	CreatedAt   time.Time              `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at" db:"updated_at"`
	DeletedAt   *time.Time             `json:"deleted_at,omitempty" db:"deleted_at"`
}
