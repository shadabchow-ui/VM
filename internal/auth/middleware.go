package auth

// middleware.go — HTTP middleware that enforces mTLS and injects host_id into request context.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6 (all inter-service communication authenticated from day 1),
//         RUNTIMESERVICE_GRPC_V1 §6 (CN=host-{host_id}),
//         IMPLEMENTATION_PLAN_V1 §R-02 (no unauthenticated internal endpoints at any milestone).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
)

type contextKey string

const hostIDKey contextKey = "authenticated_host_id"

// RequireMTLS is HTTP middleware that verifies the client presented a TLS certificate,
// extracts host_id from the certificate CN, and injects it into the request context.
// Rejects any request without a valid cert with HTTP 401.
//
// The TLS listener (CA.TLSConfig with ClientAuth=RequireAndVerifyClientCert) handles
// chain verification; this middleware handles application-level host_id extraction.
func RequireMTLS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, `{"error":{"code":"AUTH_REQUIRED","message":"mTLS client certificate required"}}`, http.StatusUnauthorized)
			return
		}

		cert := r.TLS.PeerCertificates[0]
		hostID, err := HostIDFromCert(cert)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":{"code":"AUTH_REQUIRED","message":"invalid cert CN: %s"}}`, err), http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), hostIDKey, hostID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HostIDFromCtx retrieves the authenticated host_id injected by RequireMTLS.
// Returns ("", false) if not present — callers must treat this as a hard error.
func HostIDFromCtx(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(hostIDKey).(string)
	return v, ok && v != ""
}

// NewMTLSClient builds an http.Client configured to present the given cert/key
// when connecting to the Resource Manager. Used by the Host Agent after bootstrap.
// Source: 05-02-host-runtime-worker-design.md §Steady State.
func NewMTLSClient(certPEM, keyPEM, caCertPEM []byte) (*http.Client, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("NewMTLSClient: key pair: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("NewMTLSClient: failed to parse CA cert")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}
