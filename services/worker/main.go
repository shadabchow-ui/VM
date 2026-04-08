package main

// main.go — Worker service entrypoint.
//
// Source: IMPLEMENTATION_PLAN_V1 §21.
//
// Environment:
//   DATABASE_URL              PostgreSQL DSN (required)
//   NETWORK_CONTROLLER_URL    e.g. http://network-controller.internal:8083 (required)
//   LOG_LEVEL                 debug|info|warn|error (default: info)
//
// Local-dev overrides (never set in production):
//   WORKER_IMAGE_MAP          comma-separated imageID=url pairs that override the
//                             built-in object:// image table with local file paths.
//                             Example: "00000000-...-0010=/opt/images/ubuntu.qcow2"
//   WORKER_NFS_ROOT           override the /mnt/nfs/vols default used to derive
//                             RootfsPath in CreateInstance requests. Set to a local
//                             writable directory (e.g. /tmp/dev-vols) in dev.

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/lib/pq"

	"github.com/compute-platform/compute-platform/internal/db"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
	"github.com/compute-platform/compute-platform/services/worker/handlers"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(os.Getenv("LOG_LEVEL")),
	}))

	dbURL := mustEnv("DATABASE_URL", log)
	ncURL := mustEnv("NETWORK_CONTROLLER_URL", log)

	sqlDB, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("open db", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()
	if err := sqlDB.Ping(); err != nil {
		log.Error("db ping", "error", err)
		os.Exit(1)
	}

	repo := db.New(&sqlDBPool{sqlDB})

	network := handlers.NewNetworkControllerClient(ncURL)
	deps := &handlers.Deps{
		Store:         repo,
		Network:      network,
		DefaultVPCID: "00000000-0000-0000-0000-000000000001",
		Runtime: func(hostID, address string) *runtimeclient.Client {
			return runtimeclient.NewClient(hostID, address, nil)
		},
	}

	dispatch := map[string]handlers.Handler{
		"INSTANCE_CREATE": handlers.NewCreateHandler(deps, log),
		"INSTANCE_DELETE": handlers.NewDeleteHandler(deps, log),
		// INSTANCE_START, INSTANCE_STOP, INSTANCE_REBOOT: M3 — not yet registered.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Info("worker starting",
		"handlers", []string{"INSTANCE_CREATE", "INSTANCE_DELETE"},
		"network_controller", ncURL,
	)

	loop := NewWorkerLoop(repo, sqlDB, dispatch, log)
	loop.Run(ctx)
	log.Info("worker stopped")
}

func mustEnv(key string, log *slog.Logger) string {
	v := os.Getenv(key)
	if v == "" {
		log.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// sqlDBPool adapts *sql.DB to the db.Pool interface used by Repo.
type sqlDBPool struct{ db *sql.DB }

func (p *sqlDBPool) Exec(ctx context.Context, query string, args ...any) (db.CommandTag, error) {
	r, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlTag{r}, nil
}
func (p *sqlDBPool) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRows{rows}, nil
}
func (p *sqlDBPool) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	return p.db.QueryRowContext(ctx, query, args...)
}
func (p *sqlDBPool) Close() { p.db.Close() }

type sqlTag struct{ r sql.Result }

func (t *sqlTag) RowsAffected() int64 { n, _ := t.r.RowsAffected(); return n }

type sqlRows struct{ rows *sql.Rows }

func (r *sqlRows) Next() bool             { return r.rows.Next() }
func (r *sqlRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *sqlRows) Close()                 { r.rows.Close() }
func (r *sqlRows) Err() error             { return r.rows.Err() }
