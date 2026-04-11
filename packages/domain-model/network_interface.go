package domainmodel

import "time"

type NetworkInterfaceStatus string

const (
	NetworkInterfaceStatusAttaching NetworkInterfaceStatus = "attaching"
	NetworkInterfaceStatusAttached  NetworkInterfaceStatus = "attached"
	NetworkInterfaceStatusDetaching NetworkInterfaceStatus = "detaching"
	NetworkInterfaceStatusDetached  NetworkInterfaceStatus = "detached"
	NetworkInterfaceStatusFailed    NetworkInterfaceStatus = "failed"
)

type NetworkInterface struct {
	ID          string                 `json:"id" db:"id"`
	InstanceID  string                 `json:"instance_id" db:"instance_id"`
	VPCID       string                 `json:"vpc_id" db:"vpc_id"`
	SubnetID    string                 `json:"subnet_id" db:"subnet_id"`
	PrivateIP   string                 `json:"private_ip" db:"private_ip"`
	MACAddress  string                 `json:"mac_address" db:"mac_address"`
	DeviceIndex int                    `json:"device_index" db:"device_index"`
	Status      NetworkInterfaceStatus `json:"status" db:"status"`
	CreatedAt   time.Time              `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at" db:"updated_at"`
	DeletedAt   *time.Time             `json:"deleted_at,omitempty" db:"deleted_at"`
}
