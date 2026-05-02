package metadata

// server.go — Instance metadata service at 169.254.169.254.
//
// Source: IMPLEMENTATION_PLAN_V1 §35,
//         04-03-bootstrap-initialization-and-readiness-signaling.md,
//         AUTH_OWNERSHIP_MODEL_V1 §6 (IMDSv2 token-based access).
//
// The metadata service runs on the host inside a network namespace visible to
// all VMs on that host. Each VM reaches it via the 169.254.169.254 link-local
// address, which is DNAT'd to the host's metadata service port.
//
// Endpoints:
//   PUT  /token                  { TTL-Seconds: N } → session token (string)
//   GET  /metadata/v1/ssh-key    → SSH public key for cloud-init
//   GET  /metadata/v1/user-data  → empty in Phase 1
//   GET  /metadata/v1/instance-id → the KSUID instance ID
//
// Security: all GET endpoints require X-Metadata-Token header (IMDSv2).
//
// InstanceStore interface: provides per-instance metadata lookups.
// The Host Agent main.go wires a concrete implementation backed by in-memory
// state populated during CreateInstance.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// InstanceMetadata holds the metadata for a single VM instance.
type InstanceMetadata struct {
	InstanceID   string
	SSHPublicKey string
	UserData     string // empty in Phase 1
}

// InstanceStore is the interface the metadata server uses to look up instance data.
// The Host Agent main.go provides a concrete implementation.
type InstanceStore interface {
	// GetByIP returns the InstanceMetadata for the VM with the given private IP.
	// Returns nil, false if no instance is found for that IP.
	GetByIP(privateIP string) (*InstanceMetadata, bool)
}

// Server is the HTTP server for the instance metadata service.
type Server struct {
	addr   string
	store  InstanceStore
	tokens *TokenStore
	log    *slog.Logger
}

// NewServer constructs a metadata Server.
// addr: listen address (e.g. "169.254.169.254:80" or "0.0.0.0:8080" for testing).
func NewServer(addr string, store InstanceStore, log *slog.Logger) *Server {
	return &Server{
		addr:   addr,
		store:  store,
		tokens: NewTokenStore(),
		log:    log,
	}
}

// Start registers routes and starts the HTTP server. Blocks until the server exits.
// Call in a goroutine. Returns an error only if the server fails to start.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/metadata/v1/ssh-key", s.requireToken(s.handleSSHKey))
	mux.HandleFunc("/metadata/v1/user-data", s.requireToken(s.handleUserData))
	mux.HandleFunc("/metadata/v1/instance-id", s.requireToken(s.handleInstanceID))
	mux.HandleFunc("/health", s.handleHealth)

	s.log.Info("metadata service starting", "addr", s.addr)
	return http.ListenAndServe(s.addr, mux) //nolint:gosec
}

// handleToken issues an IMDSv2 session token.
// PUT /token with X-metadata-token-ttl-seconds header or JSON body.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// TTL from header (AWS-style) takes priority over JSON body.
	ttlSeconds := 0
	if ttlStr := r.Header.Get("X-Metadata-Token-TTL-Seconds"); ttlStr != "" {
		n, err := strconv.Atoi(ttlStr)
		if err != nil || n < 1 || n > 21600 {
			http.Error(w, "X-Metadata-Token-TTL-Seconds must be 1-21600", http.StatusBadRequest)
			return
		}
		ttlSeconds = n
	}

	token, err := s.tokens.Issue(ttlSeconds)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, token)
}

// requireToken is middleware that validates the X-Metadata-Token header.
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Metadata-Token")
		if err := s.tokens.Validate(token); err != nil {
			http.Error(w, "invalid or missing X-Metadata-Token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// remoteIP extracts the client IP from the request (without port).
func remoteIP(r *http.Request) string {
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}

// instanceFromRequest resolves the InstanceMetadata for the requesting VM.
// The requesting VM's private IP is used as the lookup key.
func (s *Server) instanceFromRequest(r *http.Request) (*InstanceMetadata, bool) {
	ip := remoteIP(r)
	return s.store.GetByIP(ip)
}

func (s *Server) handleSSHKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	inst, ok := s.instanceFromRequest(r)
	if !ok {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, inst.SSHPublicKey)
}

func (s *Server) handleUserData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Phase 1: user-data is empty.
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInstanceID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	inst, ok := s.instanceFromRequest(r)
	if !ok {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": inst.InstanceID})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}
