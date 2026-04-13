package db

import "time"

// Temporary compile shim for repos missing the real project slice.
// Replace with the real project implementation when that slice is restored.
type ProjectRow struct {
	ID               string
	OwnerPrincipalID string
	PrincipalID      string
	CreatedBy        string
	Name             string
	DisplayName      string
	Description      *string
	Status           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}
