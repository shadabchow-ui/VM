package domainmodel

// elastic_ip.go — Elastic IP domain type.
//
// VM-P3A Job 2: Public connectivity maturity.
// Source: vm-14-03__blueprint__ §core_contracts "Elastic IP Allocation and Association".

import "time"

// ElasticIPAssociationType is the type of resource an EIP is currently associated with.
type ElasticIPAssociationType string

const (
	// ElasticIPAssociationNone means the EIP is allocated but not yet associated.
	ElasticIPAssociationNone ElasticIPAssociationType = "none"
	// ElasticIPAssociationNIC means the EIP is associated to a NIC (direct instance connectivity).
	ElasticIPAssociationNIC ElasticIPAssociationType = "nic"
	// ElasticIPAssociationNATGateway means the EIP is associated to a NAT Gateway.
	ElasticIPAssociationNATGateway ElasticIPAssociationType = "nat_gateway"
)

// ElasticIPStatus is the lifecycle status of an EIP.
type ElasticIPStatus string

const (
	ElasticIPStatusAvailable  ElasticIPStatus = "available"
	ElasticIPStatusAssociated ElasticIPStatus = "associated"
	ElasticIPStatusReleasing  ElasticIPStatus = "releasing"
	ElasticIPStatusReleased   ElasticIPStatus = "released"
)

// ElasticIP is the domain representation of an Elastic IP resource.
type ElasticIP struct {
	ID                   string                   `json:"id" db:"id"`
	OwnerPrincipalID     string                   `json:"owner_principal_id" db:"owner_principal_id"`
	PublicIP             string                   `json:"public_ip" db:"public_ip"`
	AssociationType      ElasticIPAssociationType `json:"association_type" db:"association_type"`
	AssociatedResourceID *string                  `json:"associated_resource_id,omitempty" db:"associated_resource_id"`
	Status               ElasticIPStatus          `json:"status" db:"status"`
	CreatedAt            time.Time                `json:"created_at" db:"created_at"`
	UpdatedAt            time.Time                `json:"updated_at" db:"updated_at"`
	DeletedAt            *time.Time               `json:"deleted_at,omitempty" db:"deleted_at"`
}
