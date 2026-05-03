package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/auth"
	runtimev1 "github.com/compute-platform/compute-platform/packages/contracts/runtimev1"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
	"github.com/compute-platform/compute-platform/services/host-agent/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// TestRuntimeMTLS_ValidCert_Succeeds proves that a worker/client with a valid
// certificate signed by the internal CA can successfully call CreateInstance
// via gRPC. This is the production happy-path.
func TestRuntimeMTLS_ValidCert_Succeeds(t *testing.T) {
	// Set up image catalog for rootfs materialization.
	nfsRoot, _ := setupImageCatalog(t)

	ca, serverCertPEM, serverKeyPEM := setupCA(t)

	// Create a client certificate signed by the same CA.
	clientCertPEM, clientKeyPEM := makeHostClientCert(t, ca, "host-test")

	// Start a gRPC server with mTLS.
	serverTLS, err := runtimeclient.BuildServerTLSConfig(serverCertPEM, serverKeyPEM, ca.CACertPEM())
	if err != nil {
		t.Fatalf("BuildServerTLSConfig: %v", err)
	}

	gs, lis, conn := startGRPCMTLSServer(t, serverTLS, nfsRoot)
	defer gs.Stop()
	defer conn.Close()

	// Build a gRPC client with the valid client cert.
	clientTLS, err := runtimeclient.BuildClientTLSConfig(clientCertPEM, clientKeyPEM, ca.CACertPEM())
	if err != nil {
		t.Fatalf("BuildClientTLSConfig: %v", err)
	}
	clientTLS.ServerName = "localhost" // match the server cert's DNS name

	grpcClient, err := runtimeclient.NewGRPCClient("host-test", "bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	defer grpcClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := grpcClient.CreateInstance(ctx, &runtimeclient.CreateInstanceRequest{
		InstanceID: "inst-mtls-001",
		CPUCores:   2,
		MemoryMB:   4096,
		ImageURL:   "object://images/ubuntu-22.04-base.qcow2",
		RootfsPath: filepath.Join(nfsRoot, "inst-mtls-001.qcow2"),
		Network: runtimeclient.NetworkConfig{
			PrivateIP:  "10.0.0.5",
			TapDevice:  "tap-mtls001",
			MacAddress: "02:00:00:00:00:01",
		},
	})

	if err != nil {
		t.Fatalf("CreateInstance with valid mTLS cert: %v", err)
	}
	if resp.InstanceID != "inst-mtls-001" {
		t.Errorf("InstanceID = %q, want inst-mtls-001", resp.InstanceID)
	}
	if resp.State != "RUNNING" {
		t.Errorf("State = %q, want RUNNING", resp.State)
	}
}

// TestRuntimeMTLS_MissingCert_Fails proves that a client without a certificate
// is rejected by the mTLS server.
func TestRuntimeMTLS_MissingCert_Fails(t *testing.T) {
	ca, serverCertPEM, serverKeyPEM := setupCA(t)

	serverTLS, err := runtimeclient.BuildServerTLSConfig(serverCertPEM, serverKeyPEM, ca.CACertPEM())
	if err != nil {
		t.Fatalf("BuildServerTLSConfig: %v", err)
	}

	gs, lis, conn := startGRPCMTLSServer(t, serverTLS, t.TempDir())
	defer gs.Stop()
	defer conn.Close()

	// Build client WITHOUT a certificate — should be rejected.
	clientTLSWithCAOnly, err := runtimeclient.BuildClientTLSConfig(nil, nil, ca.CACertPEM())
	if err != nil {
		t.Fatalf("BuildClientTLSConfig: %v", err)
	}

	grpcClient, err := runtimeclient.NewGRPCClient("host-test", "bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLSWithCAOnly)),
	)
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	defer grpcClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = grpcClient.CreateInstance(ctx, &runtimeclient.CreateInstanceRequest{
		InstanceID: "inst-mtls-002",
		CPUCores:   2,
		MemoryMB:   4096,
	})

	if err == nil {
		t.Fatal("expected error for missing client certificate, got nil")
	}
}

// TestRuntimeMTLS_InvalidCert_Fails proves that a client with a cert NOT signed
// by the internal CA is rejected.
func TestRuntimeMTLS_InvalidCert_Fails(t *testing.T) {
	ca, serverCertPEM, serverKeyPEM := setupCA(t)

	serverTLS, err := runtimeclient.BuildServerTLSConfig(serverCertPEM, serverKeyPEM, ca.CACertPEM())
	if err != nil {
		t.Fatalf("BuildServerTLSConfig: %v", err)
	}

	gs, lis, conn := startGRPCMTLSServer(t, serverTLS, t.TempDir())
	defer gs.Stop()
	defer conn.Close()

	// Create a self-signed cert NOT signed by the internal CA.
	invalidCertPEM, invalidKeyPEM := makeSelfSignedCert(t, "intruder")

	// Build a client with the invalid cert and the CA pool (which won't trust it).
	clientTLS, err := runtimeclient.BuildClientTLSConfig(invalidCertPEM, invalidKeyPEM, ca.CACertPEM())
	if err != nil {
		t.Fatalf("BuildClientTLSConfig: %v", err)
	}

	grpcClient, err := runtimeclient.NewGRPCClient("host-test", "bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	defer grpcClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = grpcClient.CreateInstance(ctx, &runtimeclient.CreateInstanceRequest{
		InstanceID: "inst-mtls-003",
		CPUCores:   2,
		MemoryMB:   4096,
	})

	if err == nil {
		t.Fatal("expected error for invalid client certificate, got nil")
	}
	// The TLS handshake should fail because the server requires a cert signed by the CA.
	if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "certificate") {
		t.Logf("Warning: error did not mention tls/certificate: %v", err)
	}
}

// TestRuntimeMTLS_PlaintextFails proves that plaintext (insecure) connections
// are rejected when the server enforces mTLS.
func TestRuntimeMTLS_PlaintextFails(t *testing.T) {
	ca, serverCertPEM, serverKeyPEM := setupCA(t)

	serverTLS, err := runtimeclient.BuildServerTLSConfig(serverCertPEM, serverKeyPEM, ca.CACertPEM())
	if err != nil {
		t.Fatalf("BuildServerTLSConfig: %v", err)
	}

	gs, lis, conn := startGRPCMTLSServer(t, serverTLS, t.TempDir())
	defer gs.Stop()
	defer conn.Close()

	// Build client with insecure credentials — should fail because server requires TLS.
	grpcClient, err := runtimeclient.NewGRPCClient("host-test", "bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	defer grpcClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = grpcClient.CreateInstance(ctx, &runtimeclient.CreateInstanceRequest{
		InstanceID: "inst-mtls-004",
		CPUCores:   2,
		MemoryMB:   4096,
	})

	if err == nil {
		t.Fatal("expected error for plaintext connection to mTLS server, got nil")
	}
}

// setupImageCatalog creates a temp directory with a valid qcow2 backing image
// and sets IMAGE_CATALOG and NETWORK_DRY_RUN environment variables.
// Returns the NFS root path and base image path.
func setupImageCatalog(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("NETWORK_DRY_RUN", "true")

	nfsRoot := t.TempDir()
	baseImage := filepath.Join(nfsRoot, "ubuntu-22.04-base.qcow2")

	if _, err := exec.LookPath("qemu-img"); err == nil {
		// Create a valid base qcow2 image.
		cmd := exec.Command("qemu-img", "create", "-f", "qcow2", baseImage, "1G")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("qemu-img create base: %v\noutput: %s", err, out)
		}
	} else {
		// Without qemu-img, CreateInstance will fail at materialization.
		// Write a dummy file and let the test handle the error gracefully.
		_ = os.WriteFile(baseImage, []byte("dummy"), 0644)
	}

	t.Setenv("IMAGE_CATALOG", "object://images/ubuntu-22.04-base.qcow2="+baseImage)
	return nfsRoot, baseImage
}

// ── Helpers (continued) ──────────────────────────────────────────────────────

func setupCA(t *testing.T) (*auth.CA, []byte, []byte) {
	t.Helper()
	ca, err := auth.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	serverCertPEM, serverKeyPEM, err := ca.GenerateServerCert([]string{"localhost"})
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}
	return ca, serverCertPEM, serverKeyPEM
}

func startGRPCMTLSServer(t *testing.T, serverTLS *tls.Config, nfsRoot string) (*grpc.Server, *bufconn.Listener, *grpc.ClientConn) {
	t.Helper()

	vm := runtime.NewFakeRuntime()
	rfs := runtime.NewRootfsManager(nfsRoot, slog.New(slog.DiscardHandler))
	netMgr := runtime.NewNetworkManager(slog.New(slog.DiscardHandler))
	svc := runtime.NewRuntimeService(vm, rfs, netMgr, slog.New(slog.DiscardHandler))
	grpcImpl := runtime.NewGRPCServer(svc, slog.New(slog.DiscardHandler))

	lis := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	runtimev1.RegisterRuntimeServiceServer(gs, grpcImpl)

	go func() {
		_ = gs.Serve(lis)
	}()

	// Return a temp conn for cleanup only (not used for calls — tests use their own dialer).
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("temp dial: %v", err)
	}

	return gs, lis, conn
}

func makeHostClientCert(t *testing.T, ca *auth.CA, hostID string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cn := "host-" + hostID
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	certPEM, err = ca.SignCSR(csrPEM, hostID)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

func makeSelfSignedCert(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-10 * time.Second),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create self-signed cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}
