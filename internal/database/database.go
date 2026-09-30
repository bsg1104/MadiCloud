// Package database owns the control-plane PostgreSQL connection pool.
//
// Callers outside this package do not import the driver. They use DB and the
// functions in this package.
package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"madicloud/internal/config"
)

// Pool limits. The control plane is one process and its queries are short, so
// a small pool leaves room on a shared PostgreSQL.
const (
	maxConns          = 10
	maxConnLifetime   = time.Hour
	maxConnIdleTime   = 5 * time.Minute
	healthCheckPeriod = time.Minute
	connectTimeout    = 5 * time.Second
	applicationName   = "madicloudd"
)

// ErrClosed is returned by operations on a DB after Close.
var ErrClosed = errors.New("database is closed")

// errInvalidConfig replaces driver parse errors, which can echo the
// connection string.
var errInvalidConfig = errors.New("invalid database connection configuration")

// DB is a PostgreSQL connection pool. It is safe for concurrent use.
type DB struct {
	pool      *pgxpool.Pool
	closed    atomic.Bool
	closeOnce sync.Once
}

// Open builds a connection pool for cfg. It does not connect: the first
// connection is made on demand, so Open succeeds while PostgreSQL is down.
// Callers must call Close.
func Open(ctx context.Context, cfg config.Database) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(connURL(cfg))
	if err != nil {
		return nil, errInvalidConfig
	}
	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = 0
	poolCfg.MaxConnLifetime = maxConnLifetime
	poolCfg.MaxConnIdleTime = maxConnIdleTime
	poolCfg.HealthCheckPeriod = healthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Ping verifies that a connection can be acquired and PostgreSQL answers.
func (db *DB) Ping(ctx context.Context) error {
	if db.closed.Load() {
		return ErrClosed
	}
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Close closes every connection and waits for acquired connections to be
// released. It is safe to call more than once.
func (db *DB) Close() {
	db.closeOnce.Do(func() {
		db.closed.Store(true)
		db.pool.Close()
	})
}

func connURL(cfg config.Database) string {
	q := url.Values{}
	q.Set("sslmode", cfg.SSLMode)
	q.Set("connect_timeout", strconv.Itoa(int(connectTimeout/time.Second)))
	q.Set("application_name", applicationName)
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:     "/" + cfg.Name,
		RawQuery: q.Encode(),
	}
	return u.String()
}
