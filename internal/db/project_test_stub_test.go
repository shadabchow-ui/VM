package db

import "time"

// Test-only compile stub.
// Replace with the real project slice when you bring it back.
type ProjectRow struct {
	ID               string
	OwnerPrincipalID string
	Name             string
	Description      *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}
