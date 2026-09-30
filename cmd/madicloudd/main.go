// Command madicloudd is the MadiCloud control plane.
//
// Usage:
//
//	madicloudd           serve the API (same as "madicloudd serve")
//	madicloudd serve     serve the API until SIGINT or SIGTERM
//	madicloudd migrate   apply pending control-plane schema migrations and exit
//
// serve starts without PostgreSQL. GET /v1/health reports process liveness and
// never touches the database; GET /v1/ready reports 503 until PostgreSQL is
// reachable and migrated. /v1/apps stores application desired state. The
// process does not schedule work, talk to nodes, or manage containers.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"madicloud/internal/api"
	"madicloud/internal/application"
	"madicloud/internal/config"
	"madicloud/internal/database"
	"madicloud/internal/version"
	"madicloud/migrations"
)

// migrateConnectTimeout bounds how long migrate waits for PostgreSQL before
// failing, so a wrong host does not hang a deploy script.
const migrateConnectTimeout = 15 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := run(ctx, os.Args[1:], os.Getenv, log)
	stop()
	if err != nil {
		log.Error("madicloudd exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, log *slog.Logger) error {
	command := "serve"
	switch len(args) {
	case 0:
	case 1:
		command = args[0]
	default:
		return fmt.Errorf("unexpected arguments %q: usage: madicloudd [serve|migrate]", args[1:])
	}
	if command != "serve" && command != "migrate" {
		return fmt.Errorf("unknown command %q: usage: madicloudd [serve|migrate]", command)
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	set, err := database.LoadMigrations(migrations.FS())
	if err != nil {
		return fmt.Errorf("embedded migrations: %w", err)
	}
	warnInsecureDatabase(cfg.Database, log)

	if command == "migrate" {
		return migrate(ctx, cfg.Database, set, log)
	}
	return serve(ctx, cfg, set, log)
}

func serve(ctx context.Context, cfg config.Config, set []database.Migration, log *slog.Logger) error {
	if !cfg.LoopbackOnly() {
		log.Warn("control plane has no authentication and is not bound to loopback", "addr", cfg.HTTPAddr)
	}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.HTTPAddr, err)
	}
	return serveListener(ctx, cfg, set, ln, log)
}

// serveListener serves on ln until ctx is cancelled. Shutdown order: stop
// accepting, drain in-flight HTTP requests (which may hold database
// connections), then close the pool. ln is closed on return.
func serveListener(ctx context.Context, cfg config.Config, set []database.Migration, ln net.Listener, log *slog.Logger) error {
	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer func() {
		db.Close()
		log.Info("database pool closed")
	}()

	ready, err := database.NewReadiness(db, set)
	if err != nil {
		_ = ln.Close()
		return err
	}
	store, err := database.NewApplicationStore(db)
	if err != nil {
		_ = ln.Close()
		return err
	}
	apps, err := application.NewService(store)
	if err != nil {
		_ = ln.Close()
		return err
	}

	log.Info("starting control plane",
		"service", version.Service,
		"version", version.Version,
		"addr", ln.Addr().String(),
		"database", cfg.Database,
		"schema_version", set[len(set)-1].Version,
	)
	return api.NewServer(cfg.HTTPAddr, version.Version, log, ready, apps).Serve(ctx, ln)
}

func migrate(ctx context.Context, cfg config.Database, set []database.Migration, log *slog.Logger) error {
	db, err := database.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(ctx, migrateConnectTimeout)
	err = db.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("database unreachable: %w", err)
	}

	log.Info("migrating control-plane schema", "database", cfg, "migrations", len(set))
	applied, err := db.Migrate(ctx, set, log)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("migration interrupted after %d applied: %w", applied, err)
		}
		return err
	}
	log.Info("control-plane schema is current", "applied", applied, "schema_version", set[len(set)-1].Version)
	return nil
}

func warnInsecureDatabase(cfg config.Database, log *slog.Logger) {
	if cfg.TLSVerified() {
		return
	}
	host := cfg.Host
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return
	}
	log.Warn("database connection does not verify the server certificate", "host", host, "sslmode", cfg.SSLMode)
}
