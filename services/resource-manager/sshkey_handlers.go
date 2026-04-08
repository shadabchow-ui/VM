package main

// sshkey_handlers.go — HTTP handlers for SSH public key management.
//
// M7 scope: Full CRUD for /v1/ssh-keys.
//
// Endpoints:
//   GET    /v1/ssh-keys           → list keys for calling principal
//   POST   /v1/ssh-keys           → add a new key (returns only fingerprint, never echoes full key)
//   DELETE /v1/ssh-keys/{key_id}  → delete key (404 on cross-account access)
//
// Source: 10-02-ssh-key-and-secret-handling.md §Public Key Storage Model,
//         AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// registerSSHKeyRoutes registers SSH key management routes.
// Called from api.go routes().
func (s *server) registerSSHKeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/ssh-keys", requirePrincipal(s.handleSSHKeyRoot))
	mux.HandleFunc("/v1/ssh-keys/", requirePrincipal(s.handleSSHKeyByID))
}

// handleSSHKeyRoot dispatches GET /v1/ssh-keys and POST /v1/ssh-keys.
func (s *server) handleSSHKeyRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListSSHKeys(w, r)
	case http.MethodPost:
		s.handleCreateSSHKey(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSSHKeyByID dispatches DELETE /v1/ssh-keys/{key_id}.
func (s *server) handleSSHKeyByID(w http.ResponseWriter, r *http.Request) {
	keyID := strings.TrimPrefix(r.URL.Path, "/v1/ssh-keys/")
	// Trim any trailing subpath — only direct IDs are supported.
	if idx := strings.Index(keyID, "/"); idx >= 0 {
		keyID = keyID[:idx]
	}
	if keyID == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.handleDeleteSSHKey(w, r, keyID)
}

// handleListSSHKeys handles GET /v1/ssh-keys.
// Returns all keys for the calling principal; never returns full public_key text.
// Source: 10-02 §Connection Credential Display.
func (s *server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	rows, err := s.repo.ListSSHKeysByPrincipal(r.Context(), principal)
	if err != nil {
		s.log.Error("ListSSHKeysByPrincipal failed", "error", err)
		writeInternalError(w)
		return
	}

	out := make([]SSHKeyResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, sshKeyToResponse(row))
	}

	writeJSON(w, http.StatusOK, ListSSHKeysResponse{
		SSHKeys: out,
		Total:   len(out),
	})
}

// handleCreateSSHKey handles POST /v1/ssh-keys.
// Validates key format and algorithm allowlist before storing.
// Returns 201 Created with fingerprint only — never echoes the full key.
// Source: 10-02 §SSH Public Key Intake and Validation.
func (s *server) handleCreateSSHKey(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	var req CreateSSHKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest, "Request body is not valid JSON.", "")
		return
	}

	if errs := validateSSHKeyRequest(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	keyType, fingerprint, err := parseSSHPublicKey(req.PublicKey)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidValue,
			fmt.Sprintf("Invalid SSH public key: %s", err.Error()), "public_key")
		return
	}

	keyID := idgen.New("sshkey")
	row := &db.SSHKeyRow{
		ID:          keyID,
		PrincipalID: principal,
		Name:        strings.TrimSpace(req.Name),
		PublicKey:   strings.TrimSpace(req.PublicKey),
		Fingerprint: fingerprint,
		KeyType:     keyType,
	}

	if err := s.repo.InsertSSHKey(r.Context(), row); err != nil {
		// Detect unique constraint violation (duplicate name or fingerprint for this principal).
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") ||
			strings.Contains(err.Error(), "UNIQUE") {
			writeAPIError(w, http.StatusConflict, errInvalidValue,
				"An SSH key with this name or fingerprint already exists.", "name")
			return
		}
		s.log.Error("InsertSSHKey failed", "error", err)
		writeInternalError(w)
		return
	}

	created, err := s.repo.GetSSHKeyByID(r.Context(), keyID)
	if err != nil {
		s.log.Error("GetSSHKeyByID after insert failed", "error", err)
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusCreated, sshKeyToResponse(created))
}

// handleDeleteSSHKey handles DELETE /v1/ssh-keys/{key_id}.
// Uses 404-on-mismatch for cross-account access — no existence leakage.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request, keyID string) {
	principal, _ := principalFromCtx(r.Context())

	// Load and verify ownership before delete.
	existing, err := s.repo.GetSSHKeyByID(r.Context(), keyID)
	if err != nil {
		if isNoRows(err) {
			writeAPIError(w, http.StatusNotFound, "ssh_key_not_found",
				"The SSH key does not exist or you do not have access to it.", "id")
			return
		}
		s.log.Error("GetSSHKeyByID failed in delete", "error", err)
		writeInternalError(w)
		return
	}

	// 404 on ownership mismatch — no existence leakage.
	if existing.PrincipalID != principal {
		writeAPIError(w, http.StatusNotFound, "ssh_key_not_found",
			"The SSH key does not exist or you do not have access to it.", "id")
		return
	}

	if err := s.repo.DeleteSSHKey(r.Context(), keyID, principal); err != nil {
		s.log.Error("DeleteSSHKey failed", "error", err)
		writeInternalError(w)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// sshKeyToResponse maps a db.SSHKeyRow to the public SSHKeyResponse.
// Never returns the full public_key text — only the fingerprint.
// Source: 10-02 §Connection Credential Display.
func sshKeyToResponse(row *db.SSHKeyRow) SSHKeyResponse {
	return SSHKeyResponse{
		ID:          row.ID,
		Name:        row.Name,
		Fingerprint: row.Fingerprint,
		KeyType:     row.KeyType,
		CreatedAt:   row.CreatedAt,
	}
}

// parseSSHPublicKey validates an SSH public key and returns its type and SHA-256 fingerprint.
//
// Algorithm allowlist: ssh-ed25519, ecdsa-sha2-nistp256/384/521.
// ssh-rsa is rejected — weak SHA-1 signature hash.
// Source: 10-02 §Algorithm Allowlist.
func parseSSHPublicKey(key string) (keyType, fingerprint string, err error) {
	key = strings.TrimSpace(key)
	parts := strings.Fields(key)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("expected format: 'key-type base64-key [comment]'")
	}

	allowedTypes := map[string]bool{
		"ssh-ed25519":         true,
		"ecdsa-sha2-nistp256": true,
		"ecdsa-sha2-nistp384": true,
		"ecdsa-sha2-nistp521": true,
	}

	kt := parts[0]
	if !allowedTypes[kt] {
		return "", "", fmt.Errorf(
			"key type %q is not allowed; accepted: ssh-ed25519, ecdsa-sha2-nistp256/384/521", kt)
	}

	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("key material is not valid base64")
	}
	if len(raw) == 0 {
		return "", "", fmt.Errorf("key material is empty")
	}

	// SHA-256 fingerprint — standard OpenSSH format.
	sum := sha256.Sum256(raw)
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])

	return kt, fp, nil
}

// validateSSHKeyRequest validates the CreateSSHKeyRequest fields.
func validateSSHKeyRequest(req *CreateSSHKeyRequest) []fieldErr {
	var errs []fieldErr

	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'name' is required.", "name"})
	} else if len(req.Name) > 255 {
		errs = append(errs, fieldErr{errInvalidValue, "Name must be 255 characters or fewer.", "name"})
	}

	if strings.TrimSpace(req.PublicKey) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'public_key' is required.", "public_key"})
	}

	return errs
}
