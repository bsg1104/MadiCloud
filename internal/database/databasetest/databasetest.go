// Package databasetest creates throwaway PostgreSQL databases for integration
// tests.
//
// Tests using it are skipped unless MADICLOUD_INTEGRATION=1. When enabled,
// connection settings come from the MADICLOUD_DB_* variables and the
// configured user must be allowed to CREATE DATABASE. An unreachable server is
// then a test failure, not a skip.
package databasetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"madicloud/internal/config"
)

// EnvEnable is the variable that turns integration tests on.
const EnvEnable = "MADICLOUD_INTEGRATION"

// Enabled reports whether integration tests should run.
func Enabled() bool { return os.Getenv(EnvEnable) == "1" }

// SkipUnlessEnabled skips t unless integration tests are enabled.
func SkipUnlessEnabled(t testing.TB) {
	t.Helper()
	if !Enabled() {
		t.Skipf("PostgreSQL integration test: set %s=1 and MADICLOUD_DB_* to run", EnvEnable)
	}
}

// New creates an empty database and returns a configuration pointing at it.
// The database is dropped when t finishes.
func New(t testing.TB) config.Database {
	t.Helper()
	SkipUnlessEnabled(t)

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		t.Fatalf("load MADICLOUD_DB_* configuration: %v", err)
	}
	admin := cfg.Database

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, connURL(admin))
	if err != nil {
		t.Fatalf("connect to PostgreSQL at %s:%d: %v", admin.Host, admin.Port, err)
	}
	defer conn.Close(context.Background())

	name := "madicloud_test_" + randomSuffix(t)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, connURL(admin))
		if err != nil {
			t.Errorf("connect to drop %s: %v", name, err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	out := admin
	out.Name = name
	return out
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

func connURL(cfg config.Database) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:     "/" + cfg.Name,
		RawQuery: url.Values{"sslmode": {cfg.SSLMode}, "connect_timeout": {"5"}}.Encode(),
	}
	return u.String()
}
