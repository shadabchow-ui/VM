package main

// vtep.go — VTEP registration and lookup endpoints for cross-host networking.
//
// M9 Slice 4: Control-plane groundwork for future VXLAN overlay networking.
//
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (VTEP management),
//         PHASE_2_MASTER_PLAN §8 (networking subsystem evolution).
//
// Exposes endpoints for host agents to register VTEP information and for
// the control plane to build/query the VTEP forwarding table:
//
//   POST /internal/v1/vtep/register     — Host agent registers its VTEP endpoint
//   POST /internal/v1/vtep/nic/register — Host agent registers a NIC for VTEP forwarding
//   GET  /internal/v1/vtep/lookup       — Lookup VTEP for a private IP in a VPC
//   GET  /internal/v1/vtep/vpc/{vpc_id} — List all VTEP entries for a VPC
//
// These are internal endpoints called by:
//   - Host agent on startup (register VTEP)
//   - Host agent on VPC instance create (register NIC)
//   - Host agent for cross-host forwarding (lookup VTEP)
//   - Network controller reconciler (list VPC entries)

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
)

// VTEPManager handles VTEP registration and lookup operations.
type VTEPManager struct {
	db  *sql.DB
	log *slog.Logger
}

// NewVTEPManager constructs a VTEPManager connected to the given database.
func NewVTEPManager(db *sql.DB, log *slog.Logger) *VTEPManager {
	return &VTEPManager{db: db, log: log}
}

// ── Host VTEP Registration ───────────────────────────────────────────────────

// RegisterHostVTEPRequest is the request body for POST /internal/v1/vtep/register.
type RegisterHostVTEPRequest struct {
	HostID        string  `json:"host_id"`
	VTEPIP        string  `json:"vtep_ip"`
	VTEPMAC       *string `json:"vtep_mac,omitempty"`
	VTEPInterface string  `json:"vtep_interface"`
}

// RegisterHostVTEPResponse is the response for host VTEP registration.
type RegisterHostVTEPResponse struct {
	HostID string `json:"host_id"`
	Status string `json:"status"`
}

// handleRegisterHostVTEP handles POST /internal/v1/vtep/register.
// Called by host agent during startup to register its VTEP endpoint.
// Source: P2_VPC_NETWORK_CONTRACT §8.2.
func (m *VTEPManager) handleRegisterHostVTEP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RegisterHostVTEPRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.HostID == "" {
		http.Error(w, "host_id is required", http.StatusBadRequest)
		return
	}
	if req.VTEPIP == "" {
		http.Error(w, "vtep_ip is required", http.StatusBadRequest)
		return
	}
	if req.VTEPInterface == "" {
		req.VTEPInterface = "vxlan0" // default
	}

	// Upsert host VTEP record
	_, err := m.db.ExecContext(r.Context(), `
		INSERT INTO host_tunnel_endpoints (
			host_id, vtep_ip, vtep_mac, vtep_interface, status, created_at, updated_at
		) VALUES ($1, $2::INET, $3::MACADDR, $4, 'active', NOW(), NOW())
		ON CONFLICT (host_id) DO UPDATE
		SET vtep_ip = EXCLUDED.vtep_ip,
		    vtep_mac = EXCLUDED.vtep_mac,
		    vtep_interface = EXCLUDED.vtep_interface,
		    status = 'active',
		    updated_at = NOW()
	`, req.HostID, req.VTEPIP, req.VTEPMAC, req.VTEPInterface)
	if err != nil {
		m.log.Error("register host VTEP failed", "host_id", req.HostID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	m.log.Info("host VTEP registered",
		"host_id", req.HostID,
		"vtep_ip", req.VTEPIP,
		"vtep_interface", req.VTEPInterface,
	)

	writeJSON(w, http.StatusOK, RegisterHostVTEPResponse{
		HostID: req.HostID,
		Status: "active",
	})
}

// ── NIC VTEP Registration ────────────────────────────────────────────────────

// RegisterNICVTEPRequest is the request body for POST /internal/v1/vtep/nic/register.
type RegisterNICVTEPRequest struct {
	RegistrationID string `json:"registration_id"` // nvr_ + KSUID
	NICID          string `json:"nic_id"`
	VPCID          string `json:"vpc_id"`
	HostID         string `json:"host_id"`
	PrivateIP      string `json:"private_ip"`
	MACAddress     string `json:"mac_address"`
	VNI            int    `json:"vni"`
}

// RegisterNICVTEPResponse is the response for NIC VTEP registration.
type RegisterNICVTEPResponse struct {
	RegistrationID string `json:"registration_id"`
	Status         string `json:"status"`
}

// handleRegisterNICVTEP handles POST /internal/v1/vtep/nic/register.
// Called by host agent when a VPC instance is created to register its NIC
// for VTEP forwarding.
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (host agent registers NIC MAC/IP).
func (m *VTEPManager) handleRegisterNICVTEP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RegisterNICVTEPRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.RegistrationID == "" || req.NICID == "" || req.VPCID == "" ||
		req.HostID == "" || req.PrivateIP == "" || req.MACAddress == "" {
		http.Error(w, "all fields are required", http.StatusBadRequest)
		return
	}
	if req.VNI < 4096 {
		http.Error(w, "vni must be >= 4096", http.StatusBadRequest)
		return
	}

	// Begin transaction: insert NIC registration + increment VPC-host attachment
	tx, err := m.db.BeginTx(r.Context(), nil)
	if err != nil {
		m.log.Error("begin tx failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Insert NIC VTEP registration
	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO nic_vtep_registrations (
			id, nic_id, vpc_id, host_id, private_ip, mac_address, vni, status, registered_at, updated_at
		) VALUES ($1, $2, $3, $4, $5::INET, $6::MACADDR, $7, 'active', NOW(), NOW())
		ON CONFLICT (id) DO UPDATE
		SET status = 'active',
		    updated_at = NOW()
	`, req.RegistrationID, req.NICID, req.VPCID, req.HostID, req.PrivateIP, req.MACAddress, req.VNI)
	if err != nil {
		m.log.Error("insert NIC VTEP registration failed", "nic_id", req.NICID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Upsert VPC-host attachment (increment instance count)
	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO vpc_host_attachments (
			id, vpc_id, host_id, instance_count, first_attached_at, last_updated_at
		) VALUES ('vha_' || substr(md5(random()::text), 1, 24), $1, $2, 1, NOW(), NOW())
		ON CONFLICT (vpc_id, host_id) DO UPDATE
		SET instance_count = vpc_host_attachments.instance_count + 1,
		    last_updated_at = NOW()
	`, req.VPCID, req.HostID)
	if err != nil {
		m.log.Error("upsert VPC-host attachment failed", "vpc_id", req.VPCID, "host_id", req.HostID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		m.log.Error("commit failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	m.log.Info("NIC VTEP registered",
		"registration_id", req.RegistrationID,
		"nic_id", req.NICID,
		"vpc_id", req.VPCID,
		"host_id", req.HostID,
		"private_ip", req.PrivateIP,
		"vni", req.VNI,
	)

	writeJSON(w, http.StatusCreated, RegisterNICVTEPResponse{
		RegistrationID: req.RegistrationID,
		Status:         "active",
	})
}

// ── VTEP Lookup ──────────────────────────────────────────────────────────────

// LookupVTEPRequest is the query parameters for GET /internal/v1/vtep/lookup.
type LookupVTEPRequest struct {
	VPCID     string `json:"vpc_id"`
	PrivateIP string `json:"private_ip"`
}

// LookupVTEPResponse is the response for VTEP lookup.
type LookupVTEPResponse struct {
	VTEPIP     string `json:"vtep_ip"`
	HostID     string `json:"host_id"`
	MACAddress string `json:"mac_address"`
	VNI        int    `json:"vni"`
}

// handleLookupVTEP handles GET /internal/v1/vtep/lookup?vpc_id=...&private_ip=...
// Returns the host VTEP IP for a private IP within a VPC.
// This is the core forwarding table lookup for cross-host traffic.
func (m *VTEPManager) handleLookupVTEP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vpcID := r.URL.Query().Get("vpc_id")
	privateIP := r.URL.Query().Get("private_ip")

	if vpcID == "" || privateIP == "" {
		http.Error(w, "vpc_id and private_ip are required", http.StatusBadRequest)
		return
	}

	var resp LookupVTEPResponse
	err := m.db.QueryRowContext(r.Context(), `
		SELECT hte.vtep_ip::TEXT, nvr.host_id, nvr.mac_address::TEXT, nvr.vni
		FROM nic_vtep_registrations nvr
		JOIN host_tunnel_endpoints hte ON nvr.host_id = hte.host_id
		WHERE nvr.vpc_id = $1
		  AND nvr.private_ip = $2::INET
		  AND nvr.status = 'active'
		  AND hte.status = 'active'
	`, vpcID, privateIP).Scan(&resp.VTEPIP, &resp.HostID, &resp.MACAddress, &resp.VNI)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "VTEP not found", http.StatusNotFound)
			return
		}
		m.log.Error("VTEP lookup failed", "vpc_id", vpcID, "private_ip", privateIP, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── List VPC VTEP Entries ────────────────────────────────────────────────────

// VTEPEntry is a single VTEP forwarding entry.
type VTEPEntry struct {
	PrivateIP  string `json:"private_ip"`
	MACAddress string `json:"mac_address"`
	HostID     string `json:"host_id"`
	VTEPIP     string `json:"vtep_ip"`
	VNI        int    `json:"vni"`
}

// ListVPCVTEPResponse is the response for listing VPC VTEP entries.
type ListVPCVTEPResponse struct {
	VPCID   string      `json:"vpc_id"`
	Entries []VTEPEntry `json:"entries"`
}

// handleListVPCVTEP handles GET /internal/v1/vtep/vpc/{vpc_id}.
// Returns all active VTEP entries for a VPC.
// Used by the network controller reconciler to build/verify VTEP tables.
func (m *VTEPManager) handleListVPCVTEP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract vpc_id from path
	path := strings.TrimPrefix(r.URL.Path, "/internal/v1/vtep/vpc/")
	vpcID := strings.TrimSuffix(path, "/")
	if vpcID == "" {
		http.Error(w, "vpc_id is required in path", http.StatusBadRequest)
		return
	}

	rows, err := m.db.QueryContext(r.Context(), `
		SELECT nvr.private_ip::TEXT, nvr.mac_address::TEXT, nvr.host_id, hte.vtep_ip::TEXT, nvr.vni
		FROM nic_vtep_registrations nvr
		JOIN host_tunnel_endpoints hte ON nvr.host_id = hte.host_id
		WHERE nvr.vpc_id = $1
		  AND nvr.status = 'active'
		  AND hte.status = 'active'
		ORDER BY nvr.registered_at ASC
	`, vpcID)
	if err != nil {
		m.log.Error("list VPC VTEP entries failed", "vpc_id", vpcID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var entries []VTEPEntry
	for rows.Next() {
		var e VTEPEntry
		if err := rows.Scan(&e.PrivateIP, &e.MACAddress, &e.HostID, &e.VTEPIP, &e.VNI); err != nil {
			m.log.Error("scan VTEP entry failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		m.log.Error("iterate VTEP entries failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, ListVPCVTEPResponse{
		VPCID:   vpcID,
		Entries: entries,
	})
}

// ── Deregister NIC VTEP ──────────────────────────────────────────────────────

// DeregisterNICVTEPRequest is the request body for POST /internal/v1/vtep/nic/deregister.
type DeregisterNICVTEPRequest struct {
	NICID  string `json:"nic_id"`
	VPCID  string `json:"vpc_id"`
	HostID string `json:"host_id"`
}

// handleDeregisterNICVTEP handles POST /internal/v1/vtep/nic/deregister.
// Called when a VPC instance is deleted to remove its NIC from the forwarding table.
func (m *VTEPManager) handleDeregisterNICVTEP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DeregisterNICVTEPRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.NICID == "" || req.VPCID == "" || req.HostID == "" {
		http.Error(w, "nic_id, vpc_id, and host_id are required", http.StatusBadRequest)
		return
	}

	// Begin transaction: mark NIC registration as removed + decrement VPC-host attachment
	tx, err := m.db.BeginTx(r.Context(), nil)
	if err != nil {
		m.log.Error("begin tx failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Mark NIC VTEP registration as removed
	_, err = tx.ExecContext(r.Context(), `
		UPDATE nic_vtep_registrations
		SET status = 'removed',
		    updated_at = NOW()
		WHERE nic_id = $1
		  AND status = 'active'
	`, req.NICID)
	if err != nil {
		m.log.Error("mark NIC VTEP removed failed", "nic_id", req.NICID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Decrement VPC-host attachment instance count
	_, err = tx.ExecContext(r.Context(), `
		UPDATE vpc_host_attachments
		SET instance_count = GREATEST(0, instance_count - 1),
		    last_updated_at = NOW()
		WHERE vpc_id = $1
		  AND host_id = $2
	`, req.VPCID, req.HostID)
	if err != nil {
		m.log.Error("decrement VPC-host attachment failed", "vpc_id", req.VPCID, "host_id", req.HostID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		m.log.Error("commit failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	m.log.Info("NIC VTEP deregistered",
		"nic_id", req.NICID,
		"vpc_id", req.VPCID,
		"host_id", req.HostID,
	)

	w.WriteHeader(http.StatusNoContent)
}
