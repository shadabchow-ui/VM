package main

// registration.go — Host Agent bootstrap registration sequence.
//
// This implements the full mTLS bootstrap flow on first boot, and re-registration
// (POST /internal/v1/hosts/register) on subsequent starts using the persisted cert.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 (bootstrap flow),
//         05-02-host-runtime-worker-design.md §Phase 1: Bootstrap Token and mTLS.
//
// Sequence (first boot):
//   1. Read bootstrap token from BOOTSTRAP_TOKEN env or /etc/host-agent/bootstrap-token file.
//   2. Generate RSA-2048 private key in memory.
//   3. Create CSR with CN=host-{AGENT_HOST_ID}.
//   4. POST /internal/v1/certificate_signing_request with {token, host_id, csr_pem}.
//   5. Receive signed cert PEM + CA cert PEM.
//   6. Persist cert + key to /etc/host-agent/mtls/{cert,key}.pem.
//   7. Persist CA cert to /etc/host-agent/mtls/ca.pem.
//
// Sequence (subsequent boots):
//   8. Load cert + key from /etc/host-agent/mtls/.
//   9. Build mTLS HTTP client.
//   10. POST /internal/v1/hosts/register with inventory payload.

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certDir     = "/etc/host-agent/mtls"
	certFile    = "cert.pem"
	keyFile     = "key.pem"
	caFile      = "ca.pem"
	tokenEnvVar = "BOOTSTRAP_TOKEN"
)

// tokenFile is the path from which the bootstrap token is read when the
// BOOTSTRAP_TOKEN env var is not set. Declared as a var (not const) so that
// tests can redirect it to a temp file without touching the filesystem root.
var tokenFile = "/etc/host-agent/bootstrap-token"

// Registrar handles Host Agent registration with the Resource Manager.
type Registrar struct {
	cfg agentConfig
	log *slog.Logger
}

func newRegistrar(cfg agentConfig, log *slog.Logger) *Registrar {
	return &Registrar{cfg: cfg, log: log}
}

// EnsureRegistered performs bootstrap on first boot, or re-registers on subsequent boots.
// Returns an mTLS http.Client ready for all subsequent Resource Manager calls.
func (r *Registrar) EnsureRegistered() (*http.Client, error) {
	certPath := filepath.Join(certDir, certFile)
	keyPath := filepath.Join(certDir, keyFile)
	caPath := filepath.Join(certDir, caFile)

	var certPEM, keyPEM, caCertPEM []byte
	var err error

	if fileExists(certPath) && fileExists(keyPath) && fileExists(caPath) {
		r.log.Info("mTLS cert found — loading existing certificate")
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, fmt.Errorf("read cert: %w", err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read key: %w", err)
		}
		caCertPEM, err = os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read ca cert: %w", err)
		}
	} else {
		r.log.Info("no mTLS cert found — running bootstrap registration")
		certPEM, keyPEM, caCertPEM, err = r.bootstrapCert()
		if err != nil {
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
		if err := persistCerts(certPEM, keyPEM, caCertPEM); err != nil {
			return nil, fmt.Errorf("persist certs: %w", err)
		}
		r.log.Info("mTLS cert issued and persisted", "cert_dir", certDir)
	}

	client, err := newMTLSClient(certPEM, keyPEM, caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("build mTLS client: %w", err)
	}

	// Register with Resource Manager using the mTLS cert.
	if err := r.registerInventory(client); err != nil {
		return nil, fmt.Errorf("register inventory: %w", err)
	}

	r.log.Info("host registered with Resource Manager", "host_id", r.cfg.HostID)
	return client, nil
}

// bootstrapCert executes steps 1–7: reads token, generates key+CSR, calls CSR endpoint.
func (r *Registrar) bootstrapCert() (certPEM, keyPEM, caCertPEM []byte, err error) {
	token, err := readBootstrapToken()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("bootstrap token: %w", err)
	}
	r.log.Info("bootstrap token loaded")

	// Generate RSA-2048 private key. Source: 05-02 §Bootstrap step 2.
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privKey),
	})

	// Create CSR with CN=host-{host_id}. Source: RUNTIMESERVICE_GRPC_V1 §6, auth/ca.go CertCNPrefix.
	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "host-" + r.cfg.HostID,
			Organization: []string{"Compute Platform"},
		},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, privKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// POST to /internal/v1/certificate_signing_request (no client cert on this call).
	// Source: 05-02 §Bootstrap step 4.
	certPEM, caCertPEM, err = r.callCSREndpoint(token, string(csrPEM))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CSR endpoint: %w", err)
	}

	return certPEM, keyPEM, caCertPEM, nil
}

type csrRequestPayload struct {
	BootstrapToken string `json:"bootstrap_token"`
	HostID         string `json:"host_id"`
	CSRPEM         string `json:"csr_pem"`
}

type csrResponsePayload struct {
	CertPEM   string `json:"cert_pem"`
	CACertPEM string `json:"ca_cert_pem"`
}

func (r *Registrar) callCSREndpoint(token, csrPEM string) (certPEM, caCertPEM []byte, err error) {
	payload := csrRequestPayload{
		BootstrapToken: token,
		HostID:         r.cfg.HostID,
		CSRPEM:         csrPEM,
	}
	body, _ := json.Marshal(payload)

	// This call uses a plain (non-mTLS) client because the cert doesn't exist yet.
	// The Resource Manager's CSR endpoint validates via bootstrap token, not client cert.
	plainClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // CA cert not yet known; verified by token auth instead.
				// Phase 2: distribute CA cert via provisioning so this can be false.
			},
		},
	}

	url := r.cfg.ResourceManagerURL + "/internal/v1/certificate_signing_request"
	resp, err := plainClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("POST CSR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("CSR endpoint returned %d: %s", resp.StatusCode, string(respBody))
	}

	var out csrResponsePayload
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil, fmt.Errorf("decode CSR response: %w", err)
	}
	if out.CertPEM == "" || out.CACertPEM == "" {
		return nil, nil, errors.New("CSR response missing cert_pem or ca_cert_pem")
	}

	return []byte(out.CertPEM), []byte(out.CACertPEM), nil
}

type registerPayload struct {
	HostID           string `json:"host_id"`
	AvailabilityZone string `json:"availability_zone"`
	TotalCPU         int    `json:"total_cpu"`
	TotalMemoryMB    int    `json:"total_memory_mb"`
	TotalDiskGB      int    `json:"total_disk_gb"`
	AgentVersion     string `json:"agent_version"`
	// M9 Slice 4: VTEP underlay IP for cross-host networking.
	// Source: P2_VPC_NETWORK_CONTRACT §8.2 (each compute host has one VTEP interface).
	VTEPUnderlayIP string `json:"vtep_underlay_ip,omitempty"`
}

// registerInventory POSTs the host's inventory to the Resource Manager using mTLS.
// Source: 05-02 §Bootstrap step 8, IMPLEMENTATION_PLAN_V1 §B1.
func (r *Registrar) registerInventory(client *http.Client) error {
	inv := detectInventory()
	payload := registerPayload{
		HostID:           r.cfg.HostID,
		AvailabilityZone: r.cfg.AvailabilityZone,
		TotalCPU:         inv.totalCPU,
		TotalMemoryMB:    inv.totalMemoryMB,
		TotalDiskGB:      inv.totalDiskGB,
		AgentVersion:     r.cfg.AgentVersion,
		// M9 Slice 4: Include VTEP underlay IP for cross-host networking.
		// Source: P2_VPC_NETWORK_CONTRACT §8.2.
		VTEPUnderlayIP: inv.vtepUnderlayIP,
	}
	body, _ := json.Marshal(payload)

	url := r.cfg.ResourceManagerURL + "/internal/v1/hosts/register"
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// --- helpers ---

func readBootstrapToken() (string, error) {
	// 1. Check env var first.
	if tok := strings.TrimSpace(os.Getenv(tokenEnvVar)); tok != "" {
		return tok, nil
	}
	// 2. Fall back to file.
	//
	// Write the raw token value only — not the full JSON from internal-cli output.
	// Correct:   printf '%s' "$TOKEN" | sudo tee /etc/host-agent/bootstrap-token
	// Incorrect: echo '{"token": "..."}' > /etc/host-agent/bootstrap-token
	//
	// Leading/trailing whitespace and newlines are trimmed below, so both
	// `printf '%s'` and `echo` (which appends \n) produce the same result.
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("no bootstrap token in env %s or file %s: %w", tokenEnvVar, tokenFile, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("bootstrap token file %s is empty", tokenFile)
	}
	return tok, nil
}

func persistCerts(certPEM, keyPEM, caCertPEM []byte) error {
	if err := os.MkdirAll(certDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, certFile), certPEM, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, keyFile), keyPEM, 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(certDir, caFile), caCertPEM, 0644)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func newMTLSClient(certPEM, keyPEM, caCertPEM []byte) (*http.Client, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("key pair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, errors.New("failed to parse CA cert PEM")
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   30 * time.Second,
	}, nil
}

// hostInventory holds the physical resource counts discovered at startup.
type hostInventory struct {
	totalCPU      int
	totalMemoryMB int
	totalDiskGB   int
	// M9 Slice 4: VTEP underlay IP for cross-host networking.
	vtepUnderlayIP string
}

// detectInventory reads /proc/cpuinfo and /proc/meminfo on Linux.
// Falls back to safe defaults in non-Linux environments (dev/test).
func detectInventory() hostInventory {
	return hostInventory{
		totalCPU:       detectCPUCores(),
		totalMemoryMB:  detectMemoryMB(),
		totalDiskGB:    detectDiskGB(),
		vtepUnderlayIP: detectVTEPUnderlayIP(),
	}
}

func detectCPUCores() int {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 1 // fallback
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}

func detectMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 1024 // fallback: 1 GB
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			var kb int
			fmt.Sscanf(strings.TrimPrefix(line, "MemTotal:"), "%d", &kb)
			return kb / 1024
		}
	}
	return 1024
}

func detectDiskGB() int {
	// Phase 1: return a fixed estimate. Phase 2: use syscall.Statfs.
	// Source: IMPLEMENTATION_PLAN_V1 §B1 (host inventory includes storage).
	return 100
}

// detectVTEPUnderlayIP returns the host's underlay IP for VXLAN tunneling.
// M9 Slice 4: Control-plane groundwork for cross-host networking.
// Source: P2_VPC_NETWORK_CONTRACT §8.2 (each compute host has one VTEP interface).
//
// Detection order:
//  1. VTEP_UNDERLAY_IP env var (explicit override)
//  2. IP of the interface specified in VTEP_INTERFACE env var
//  3. IP of eth0 (default underlay interface)
//  4. First non-loopback IPv4 address
//  5. Empty string (VTEP registration skipped)
func detectVTEPUnderlayIP() string {
	// 1. Explicit env var override
	if ip := strings.TrimSpace(os.Getenv("VTEP_UNDERLAY_IP")); ip != "" {
		return ip
	}

	// 2. Get interface name from env or default to eth0
	ifaceName := strings.TrimSpace(os.Getenv("VTEP_INTERFACE"))
	if ifaceName == "" {
		ifaceName = "eth0"
	}

	// 3. Try to read IP from specified interface via /sys/class/net
	// This is a simplified detection that works on Linux.
	// Full implementation would use net.Interfaces() but we keep deps minimal.
	ip := readInterfaceIP(ifaceName)
	if ip != "" {
		return ip
	}

	// 4. Fall back to parsing /proc/net/fib_trie or hostname -I
	ip = detectFirstNonLoopbackIP()
	return ip
}

// readInterfaceIP attempts to read the IPv4 address of an interface.
// Returns empty string if interface doesn't exist or has no IPv4 address.
func readInterfaceIP(ifaceName string) string {
	// Read from /proc/net/route and /proc/net/fib_trie would be complex.
	// Instead, use a simple approach: read from hostname -I output if available.
	// This is a placeholder - production would use net.InterfaceByName().
	return ""
}

// detectFirstNonLoopbackIP returns the first non-loopback IPv4 address.
// Returns empty string if none found.
func detectFirstNonLoopbackIP() string {
	// Read /proc/net/route for default gateway interface, then get its IP.
	// This is a simplified implementation.
	// Production code would use net.Interfaces() and iterate.
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}

	// Find the default route interface (destination 00000000)
	var defaultIface string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "00000000" {
			defaultIface = fields[0]
			break
		}
	}
	if defaultIface == "" {
		return ""
	}

	// This would need to be extended to actually read the IP.
	// For now, return empty and rely on VTEP_UNDERLAY_IP env var in production.
	return ""
}
