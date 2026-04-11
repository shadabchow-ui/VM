package domainmodel

import "time"

// HostTunnelEndpointStatus represents the status of a host's VTEP.
type HostTunnelEndpointStatus string

const (
	HostTunnelEndpointStatusActive   HostTunnelEndpointStatus = "active"
	HostTunnelEndpointStatusDraining HostTunnelEndpointStatus = "draining"
	HostTunnelEndpointStatusOffline  HostTunnelEndpointStatus = "offline"
)

// HostTunnelEndpoint represents a host's VXLAN Tunnel Endpoint (VTEP) identity.
// Each compute host has one VTEP interface bound to its physical NIC.
// The network controller uses this to build the VTEP routing table for
// cross-host VPC traffic.
//
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (VTEP management).
type HostTunnelEndpoint struct {
	HostID        string                   `json:"host_id" db:"host_id"`
	VTEPIP        string                   `json:"vtep_ip" db:"vtep_ip"`
	VTEPMAC       *string                  `json:"vtep_mac,omitempty" db:"vtep_mac"`
	VTEPInterface string                   `json:"vtep_interface" db:"vtep_interface"`
	Status        HostTunnelEndpointStatus `json:"status" db:"status"`
	CreatedAt     time.Time                `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time                `json:"updated_at" db:"updated_at"`
}
