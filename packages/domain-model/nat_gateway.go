package domainmodel

// nat_gateway.go — NAT Gateway domain type.
//
// VM-P3A Job 2: Public connectivity maturity.
// Source: vm-14-03__blueprint__ §core_contracts "NAT Gateway Lifecycle".

import "time"

// NATGatewayStatus is the lifecycle status of a NAT Gateway.
type NATGatewayStatus string

const (
	NATGatewayStatusPending   NATGatewayStatus = "pending"
	NATGatewayStatusAvailable NATGatewayStatus = "available"
	NATGatewayStatusDeleting  NATGatewayStatus = "deleting"
	NATGatewayStatusDeleted   NATGatewayStatus = "deleted"
	NATGatewayStatusFailed    NATGatewayStatus = "failed"
)

// NATGateway is the domain representation of a NAT Gateway resource.
// A NAT Gateway enables outbound internet traffic from private subnets.
// It is subnet-scoped and requires one EIP for outbound SNAT.
type NATGateway struct {
	ID               string           `json:"id" db:"id"`
	OwnerPrincipalID string           `json:"owner_principal_id" db:"owner_principal_id"`
	VPCID            string           `json:"vpc_id" db:"vpc_id"`
	SubnetID         string           `json:"subnet_id" db:"subnet_id"`
	ElasticIPID      string           `json:"elastic_ip_id" db:"elastic_ip_id"`
	Status           NATGatewayStatus `json:"status" db:"status"`
	CreatedAt        time.Time        `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at" db:"updated_at"`
	DeletedAt        *time.Time       `json:"deleted_at,omitempty" db:"deleted_at"`
}
