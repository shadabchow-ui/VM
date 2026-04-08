package auth

// ca.go — Internal Certificate Authority for Host Agent mTLS bootstrap.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 (mTLS bootstrap flow),
//         05-02-host-runtime-worker-design.md §Phase 1: Bootstrap Token and mTLS,
//         RUNTIMESERVICE_GRPC_V1 §6 (cert CN format: host-{host_id}).
//
// Flow:
//   1. control plane calls NewCA() at startup — generates or loads a self-signed CA.
//   2. Host Agent boots, reads bootstrap token from env/file.
//   3. Host Agent generates RSA-2048 key pair, creates CSR with CN=host-{host_id}.
//   4. Host Agent POSTs {bootstrap_token, csr_pem} to /internal/v1/certificate_signing_request.
//   5. Resource Manager calls CA.SignCSR(tokenHash, csrPEM) → signed cert PEM.
//   6. Host Agent persists cert+key, switches all subsequent calls to mTLS.
//
// The CA private key never leaves the Resource Manager process memory in Phase 1.
// Phase 2: replace with Vault PKI or managed CA service.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// CertCNPrefix is prepended to host_id in the mTLS certificate CN.
	// Source: RUNTIMESERVICE_GRPC_V1 §6, AUTH_OWNERSHIP_MODEL_V1 §6.
	CertCNPrefix = "host-"

	// CertValidityDuration is how long a host mTLS cert is valid.
	// Long-lived for Phase 1. Phase 2: shorten and add rotation.
	CertValidityDuration = 365 * 24 * time.Hour

	// CAValidityDuration is how long the internal CA cert is valid.
	CAValidityDuration = 10 * 365 * 24 * time.Hour
)

// CA holds the internal certificate authority used to sign Host Agent certs.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *rsa.PrivateKey
}

// LoadOrCreateCA loads the CA from certPath/keyPath if both files exist,
// or generates a new CA and persists it to those paths.
// This is the correct call site for Resource Manager startup in Phase 1.
// Phase 2: replace with Vault PKI or managed CA.
func LoadOrCreateCA(certPath, keyPath string) (*CA, error) {
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		ca, err := LoadCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("LoadOrCreateCA: persisted CA is corrupt, remove %s and %s to regenerate: %w", certPath, keyPath, err)
		}
		return ca, nil
	}
	ca, err := NewCA()
	if err != nil {
		return nil, err
	}
	if err := ca.Save(certPath, keyPath); err != nil {
		return nil, fmt.Errorf("LoadOrCreateCA: failed to persist CA: %w", err)
	}
	return ca, nil
}

// Save persists the CA cert and key PEM to disk.
// The directory must already exist or be creatable.
// File permissions: cert 0644, key 0600.
func (ca *CA) Save(certPath, keyPath string) error {
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(ca.key),
	})
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return fmt.Errorf("CA Save mkdir: %w", err)
	}
	if err := os.WriteFile(certPath, ca.certPEM, 0644); err != nil {
		return fmt.Errorf("CA Save cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("CA Save key: %w", err)
	}
	return nil
}

// NewCA generates a new self-signed CA. Call once at Resource Manager startup.
// In production, persist caKeyPEM + caCertPEM to a secret store and reload on restart.
func NewCA() (*CA, error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, fmt.Errorf("CA key generation: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("CA serial generation: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization:       []string{"Compute Platform"},
			OrganizationalUnit: []string{"Internal CA"},
			CommonName:         "compute-platform-internal-ca",
		},
		NotBefore:             time.Now().Add(-10 * time.Second),
		NotAfter:              time.Now().Add(CAValidityDuration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("CA cert creation: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("CA cert parse: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	return &CA{cert: cert, certPEM: certPEM, key: key}, nil
}

// LoadCA reconstructs a CA from persisted PEM-encoded cert and key.
// Use this path when the Resource Manager restarts.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("LoadCA: invalid cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("LoadCA cert parse: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("LoadCA: invalid key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("LoadCA key parse: %w", err)
	}

	return &CA{cert: cert, certPEM: certPEM, key: key}, nil
}

// CACertPEM returns the CA certificate in PEM format.
// Distributed to Host Agents so they can verify the server's identity.
func (ca *CA) CACertPEM() []byte {
	return ca.certPEM
}

// SignCSR signs a PEM-encoded CSR and returns a PEM-encoded client certificate.
// The CN in the CSR must match "host-{expectedHostID}" — enforced here.
//
// Called by the Resource Manager's /internal/v1/certificate_signing_request handler
// after ConsumeBootstrapToken succeeds.
//
// Source: 05-02-host-runtime-worker-design.md §Bootstrap step 3,
//         RUNTIMESERVICE_GRPC_V1 §6.
func (ca *CA) SignCSR(csrPEM []byte, expectedHostID string) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("SignCSR: invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("SignCSR parse: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("SignCSR signature check: %w", err)
	}

	// Enforce CN format. Source: RUNTIMESERVICE_GRPC_V1 §6.
	expectedCN := CertCNPrefix + expectedHostID
	if csr.Subject.CommonName != expectedCN {
		return nil, fmt.Errorf("SignCSR CN mismatch: got %q, want %q", csr.Subject.CommonName, expectedCN)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("SignCSR serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		NotBefore:    time.Now().Add(-10 * time.Second),
		NotAfter:     time.Now().Add(CertValidityDuration),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("SignCSR create: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), nil
}

// TLSConfig returns the tls.Config for the Resource Manager's mTLS listener.
// Requires client certs signed by this CA. Extracts host_id from CN for each request.
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 (all inter-service communication authenticated from day 1).
func (ca *CA) TLSConfig(serverCertPEM, serverKeyPEM []byte) (*tls.Config, error) {
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("TLSConfig: server key pair: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// HostIDFromCert extracts the host_id from an mTLS client certificate's CN.
// CN format: "host-{host_id}". Returns an error if the CN does not match the prefix.
// Source: RUNTIMESERVICE_GRPC_V1 §6.
func HostIDFromCert(cert *x509.Certificate) (string, error) {
	cn := cert.Subject.CommonName
	if !strings.HasPrefix(cn, CertCNPrefix) {
		return "", fmt.Errorf("HostIDFromCert: CN %q does not have prefix %q", cn, CertCNPrefix)
	}
	hostID := strings.TrimPrefix(cn, CertCNPrefix)
	if hostID == "" {
		return "", fmt.Errorf("HostIDFromCert: CN %q has empty host_id after prefix", cn)
	}
	return hostID, nil
}

func (ca *CA) GenerateServerCert(dnsNames []string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("server key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("server serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "compute-platform-resource-manager"},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-10 * time.Second),
		NotAfter:     time.Now().Add(CertValidityDuration),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, fmt.Errorf("server cert: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}
