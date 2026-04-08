package auth_test

// ca_test.go — Unit tests for the internal CA.
// No database required. Run: go test ./internal/auth/...
//
// Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests (auth enforced on host↔control-plane path).

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compute-platform/compute-platform/internal/auth"
)

func TestCA_NewCA(t *testing.T) {
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM := ca.CACertPEM()
	if len(certPEM) == 0 {
		t.Fatal("CACertPEM returned empty bytes")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CACertPEM is not a valid CERTIFICATE PEM block, got type %q", func() string {
			if block == nil {
				return "<nil>"
			}
			return block.Type
		}())
	}
}

func TestCA_SignCSR(t *testing.T) {
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	hostID := "abc123"
	csrPEM := makeCSR(t, "host-"+hostID)

	certPEM, err := ca.SignCSR(csrPEM, hostID)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("signed cert is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse signed cert: %v", err)
	}
	if cert.Subject.CommonName != "host-"+hostID {
		t.Errorf("CN: got %q, want %q", cert.Subject.CommonName, "host-"+hostID)
	}
}

// TestCA_SignCSR_RejectsBadCN verifies CN mismatch between CSR and expected host_id is caught.
// Source: RUNTIMESERVICE_GRPC_V1 §6 (CN must be host-{host_id}).
func TestCA_SignCSR_RejectsBadCN(t *testing.T) {
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	csrPEM := makeCSR(t, "host-wrong-id")
	_, err = ca.SignCSR(csrPEM, "correct-id")
	if err == nil {
		t.Fatal("expected CN mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "CN mismatch") {
		t.Errorf("expected 'CN mismatch' in error, got: %v", err)
	}
}

func TestCA_SignCSR_RejectsMissingPrefix(t *testing.T) {
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	// CSR CN has no "host-" prefix.
	csrPEM := makeCSR(t, "abc123")
	_, err = ca.SignCSR(csrPEM, "abc123")
	if err == nil {
		t.Fatal("expected error for missing prefix, got nil")
	}
}

func TestCA_SignCSR_RejectsInvalidPEM(t *testing.T) {
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	_, err = ca.SignCSR([]byte("not valid pem"), "some-host")
	if err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
}

func TestCA_HostIDFromCert_HappyPath(t *testing.T) {
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	hostID := "myhost42"
	certPEM, err := ca.SignCSR(makeCSR(t, "host-"+hostID), hostID)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)

	got, err := auth.HostIDFromCert(cert)
	if err != nil {
		t.Fatalf("HostIDFromCert: %v", err)
	}
	if got != hostID {
		t.Errorf("got %q, want %q", got, hostID)
	}
}

func TestCA_HostIDFromCert_RejectsBadCN(t *testing.T) {
	// Build a cert with a non-host CN directly (bypassing SignCSR's prefix check).
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "not-a-host"},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(certDER)

	_, err := auth.HostIDFromCert(cert)
	if err == nil {
		t.Fatal("expected error for non-host CN, got nil")
	}
}

func TestCA_TLSConfig_RequiresClientAuth(t *testing.T) {
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := ca.GenerateServerCert([]string{"localhost"})
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}
	tlsCfg, err := ca.TLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth: got %v, want RequireAndVerifyClientCert", tlsCfg.ClientAuth)
	}
}


// TestCA_SaveAndLoadOrCreate_RoundTrip verifies that:
//  1. LoadOrCreateCA generates a CA and persists cert+key on first call.
//  2. A second call loads the same CA (same public key bytes) rather than
//     generating a new one.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 — CA must be stable across restarts so
// that host-agent trust roots remain valid after Resource Manager is restarted.
func TestCA_SaveAndLoadOrCreate_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")

	// ── First call: files do not exist → generate + save ─────────────────────
	ca1, err := auth.LoadOrCreateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreateCA (first): %v", err)
	}

	// Files must now exist on disk.
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("cert file not written: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key file not written: %v", err)
	}

	// ── Second call: files exist → load (must be the same CA) ────────────────
	ca2, err := auth.LoadOrCreateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreateCA (second): %v", err)
	}

	// The CA cert PEM must be byte-for-byte identical between the two calls.
	if string(ca1.CACertPEM()) != string(ca2.CACertPEM()) {
		t.Error("CACertPEM differs between first and second LoadOrCreateCA — a new CA was generated on the second call")
	}
}

// TestCA_Save_FilePermissions verifies that the persisted key file is written
// with mode 0600 (owner-read/write only) so the CA private key is not world-readable.
func TestCA_Save_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")

	_, err := auth.LoadOrCreateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Errorf("key file mode: got %04o, want 0600", keyInfo.Mode().Perm())
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert file: %v", err)
	}
	if certInfo.Mode().Perm() != 0644 {
		t.Errorf("cert file mode: got %04o, want 0644", certInfo.Mode().Perm())
	}
}

// TestCA_LoadOrCreate_CorruptFileReturnsError verifies that corrupt on-disk CA
// files return an error rather than silently generating a new (mismatched) CA.
func TestCA_LoadOrCreate_CorruptFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca.key")

	// Write garbage so both files exist but are invalid PEM.
	if err := os.WriteFile(certPath, []byte("not valid pem"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("not valid pem"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := auth.LoadOrCreateCA(certPath, keyPath)
	if err == nil {
		t.Fatal("expected error for corrupt CA files, got nil")
	}
}

// makeCSR generates a PEM-encoded CSR with the given CN.
func makeCSR(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
}
