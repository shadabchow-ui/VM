package main

// main.go — Resource Manager service entrypoint.
//
// Start sequence:
//   1. Validate config from environment.
//   2. Connect to PostgreSQL via db.NewSQLPool, ping.
//   3. Generate internal CA (Phase 1: in-memory).
//   4. Generate self-signed server TLS cert.
//   5. Start mTLS HTTP listener.
//   6. Block on SIGTERM/SIGINT → graceful shutdown.
//
// Source: IMPLEMENTATION_PLAN_V1 §B2, AUTH_OWNERSHIP_MODEL_V1 §6 (R-02).

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/compute-platform/compute-platform/internal/auth"
	"github.com/compute-platform/compute-platform/internal/db"
)

type server struct {
	inventory *HostInventory
	ca        *auth.CA
	log       *slog.Logger
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := loadConfig()

	// ── PostgreSQL ─────────────────────────────────────────────────────────
	// Uses database/sql + lib/pq. On the real machine with pgx in go.sum,
	// replace with:  pool, _ := pgxpool.New(ctx, cfg.DatabaseURL)
	pool, err := db.NewSQLPool(cfg.DatabaseURL)
	if err != nil {
		log.Error("DB connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	repo := db.New(pool)
	log.Info("PostgreSQL connected")

	// ── Certificate Authority ───────────────────────────────────────────────
	// LoadOrCreateCA: loads the CA from disk if it already exists, or generates
	// a new one and persists it. This ensures the CA identity is stable across
	// restarts — host-agents that have already bootstrapped continue to work.
	// Phase 2: replace these paths with Vault PKI or a managed secret store.
	ca, err := auth.LoadOrCreateCA(cfg.CACertPath, cfg.CAKeyPath)
	if err != nil {
		log.Error("CA init failed", "error", err)
		os.Exit(1)
	}
	log.Info("internal CA initialized", "ca_cert", cfg.CACertPath)

	// ── Server TLS cert ─────────────────────────────────────────────────────
	serverCertPEM, serverKeyPEM, err := ca.GenerateServerCert([]string{cfg.DNSName, "localhost"})
	if err != nil {
		log.Error("server cert generation failed", "error", err)
		os.Exit(1)
	}
	tlsCfg, err := ca.TLSConfig(serverCertPEM, serverKeyPEM)
	if err != nil {
		log.Error("TLS config failed", "error", err)
		os.Exit(1)
	}

	// Bootstrap exception: allow CSR bootstrap before the host has a client cert.
	// Protected routes still require mTLS via auth.RequireMTLS middleware.
	tlsCfg.ClientAuth = tls.VerifyClientCertIfGiven

	// Bootstrap exception: allow CSR bootstrap before the host has a client cert.
	// Protected routes still require mTLS via auth.RequireMTLS middleware.

	// Bootstrap exception:
	// allow the CSR bootstrap endpoint to connect before the host has a client cert.
	// Protected routes still require mTLS via auth.RequireMTLS middleware.

	// Bootstrap exception:
	// /internal/v1/certificate_signing_request must be reachable before the host
	// has a client certificate. All protected routes still enforce mTLS via
	// auth.RequireMTLS at the HTTP layer.

	// ── Wire server ─────────────────────────────────────────────────────────
	srv := &server{
		inventory: newHostInventory(repo),
		ca:        ca,
		log:       log,
	}
	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen failed", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}
	tlsLn := tls.NewListener(ln, tlsCfg)

	log.Info("Resource Manager starting",
		"addr", cfg.ListenAddr,
		"auth", "CSR endpoint allows no client cert; protected routes require mTLS",
	)

	go func() {
		if err := httpSrv.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// ── Graceful shutdown ────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	log.Info("shutdown signal received", "signal", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Error("shutdown error", "error", err)
	}
	log.Info("Resource Manager stopped")
}

// ── Config ────────────────────────────────────────────────────────────────────

type config struct {
	DatabaseURL string
	ListenAddr  string
	DNSName     string
	// CACertPath / CAKeyPath: where the internal CA cert and key are persisted.
	// Default: /etc/resource-manager/ca/ca.{pem,key}.
	// On first start these files don't exist — the CA is generated and saved.
	// On subsequent starts the existing CA is loaded, keeping the trust root stable.
	CACertPath string
	CAKeyPath  string
}

func loadConfig() config {
	cfg := config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		ListenAddr:  envOr("RESOURCE_MANAGER_ADDR", ":9090"),
		DNSName:     envOr("RESOURCE_MANAGER_DNS", "resource-manager.internal"),
		CACertPath:  envOr("CA_CERT_PATH", "/etc/resource-manager/ca/ca.pem"),
		CAKeyPath:   envOr("CA_KEY_PATH", "/etc/resource-manager/ca/ca.key"),
	}
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
