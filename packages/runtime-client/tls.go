package runtimeclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// BuildClientTLSConfig returns a tls.Config for the worker → host-agent gRPC path.
// The worker presents a client certificate signed by the internal CA.
// The host-agent's server cert must be signed by the same CA.
//
// certPEM / keyPEM: the worker's own client certificate and key (PEM-encoded).
// caCertPEM: the internal CA certificate (PEM-encoded).
//
// If certPEM or keyPEM is empty, no client certificate is presented (used in dev).
func BuildClientTLSConfig(certPEM, keyPEM, caCertPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("tls: failed to append CA certificate to pool")
	}

	cfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS13,
	}

	if len(certPEM) > 0 && len(keyPEM) > 0 {
		clientCert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("tls: client key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{clientCert}
	}

	return cfg, nil
}

// LoadClientCertFromFiles reads cert and key PEM from disk and returns
// grpc.DialOption for mTLS authentication.
//
// certPath: path to the client certificate PEM file
// keyPath: path to the client key PEM file
// caCertPath: path to the CA certificate PEM file
//
// If certPath or keyPath is empty, returns a dial option with only CA verification
// (no client certificate). Use this for dev/test scenarios.
func LoadClientCertFromFiles(certPath, keyPath, caCertPath string) (grpc.DialOption, error) {
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("tls: read CA cert: %w", err)
	}

	var certPEM, keyPEM []byte
	if certPath != "" && keyPath != "" {
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, fmt.Errorf("tls: read client cert: %w", err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("tls: read client key: %w", err)
		}
	}

	tlsCfg, err := BuildClientTLSConfig(certPEM, keyPEM, caCertPEM)
	if err != nil {
		return nil, err
	}

	return grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), nil
}

// BuildServerTLSConfig returns a tls.Config for the host-agent gRPC server.
// The server presents its own certificate and requires client certificates
// signed by the internal CA.
//
// serverCertPEM / serverKeyPEM: the host-agent's server certificate and key.
// caCertPEM: the internal CA certificate for verifying client certs.
func BuildServerTLSConfig(serverCertPEM, serverKeyPEM, caCertPEM []byte) (*tls.Config, error) {
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("tls: server key pair: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("tls: failed to append CA certificate to pool")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
