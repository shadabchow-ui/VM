package db

// sshkey_repo.go — SSH public key persistence methods.
//
// M7: Added to support /v1/ssh-keys CRUD endpoints.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §5, 10-02-ssh-key-and-secret-handling.md,
//         db/migrations/001_initial.up.sql §10 (ssh_public_keys table).

import (
	"context"
	"fmt"
	"time"
)

// SSHKeyRow is the DB representation of an ssh_public_keys row.
type SSHKeyRow struct {
	ID          string
	PrincipalID string
	Name        string
	PublicKey   string
	Fingerprint string
	KeyType     string
	CreatedAt   time.Time
}

// InsertSSHKey inserts a new SSH public key for a principal.
// Returns an error on unique constraint violation (duplicate name or fingerprint).
func (r *Repo) InsertSSHKey(ctx context.Context, row *SSHKeyRow) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO ssh_public_keys (id, principal_id, name, public_key, fingerprint, key_type, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, row.ID, row.PrincipalID, row.Name, row.PublicKey, row.Fingerprint, row.KeyType)
	if err != nil {
		return fmt.Errorf("InsertSSHKey: %w", err)
	}
	return nil
}

// ListSSHKeysByPrincipal returns all SSH keys for a principal, newest first.
func (r *Repo) ListSSHKeysByPrincipal(ctx context.Context, principalID string) ([]*SSHKeyRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, principal_id, name, public_key, fingerprint, key_type, created_at
		FROM ssh_public_keys
		WHERE principal_id = $1
		ORDER BY created_at DESC
	`, principalID)
	if err != nil {
		return nil, fmt.Errorf("ListSSHKeysByPrincipal: %w", err)
	}
	defer rows.Close()

	var out []*SSHKeyRow
	for rows.Next() {
		row := &SSHKeyRow{}
		if err := rows.Scan(
			&row.ID, &row.PrincipalID, &row.Name,
			&row.PublicKey, &row.Fingerprint, &row.KeyType, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("ListSSHKeysByPrincipal scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetSSHKeyByID fetches a single SSH key by ID.
func (r *Repo) GetSSHKeyByID(ctx context.Context, id string) (*SSHKeyRow, error) {
	row := &SSHKeyRow{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, principal_id, name, public_key, fingerprint, key_type, created_at
		FROM ssh_public_keys
		WHERE id = $1
	`, id).Scan(
		&row.ID, &row.PrincipalID, &row.Name,
		&row.PublicKey, &row.Fingerprint, &row.KeyType, &row.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("GetSSHKeyByID %s: %w", id, err)
	}
	return row, nil
}

// DeleteSSHKey removes an SSH key by ID scoped to a principal.
// Idempotent: 0 rows affected is not an error.
func (r *Repo) DeleteSSHKey(ctx context.Context, id, principalID string) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM ssh_public_keys
		WHERE id = $1 AND principal_id = $2
	`, id, principalID)
	if err != nil {
		return fmt.Errorf("DeleteSSHKey: %w", err)
	}
	return nil
}
