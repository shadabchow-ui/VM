package domainmodel

import "time"

type SecurityGroup struct {
	ID               string     `json:"id" db:"id"`
	OwnerPrincipalID string     `json:"owner_principal_id" db:"owner_principal_id"`
	VPCID            string     `json:"vpc_id" db:"vpc_id"`
	Name             string     `json:"name" db:"name"`
	Description      *string    `json:"description,omitempty" db:"description"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" db:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty" db:"deleted_at"`
}
