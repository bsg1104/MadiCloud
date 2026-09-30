package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// migrationLockKey is the pg_advisory_lock key held while migrating, so
// concurrent migrators in the same database run one at a time.
const migrationLockKey int64 = 0x6d6164696d696772 // "madimigr"

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    bigint      PRIMARY KEY CHECK (version > 0),
    name       text        NOT NULL,
    checksum   text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

var migrationFileName = regexp.MustCompile(`^([0-9]{4,})_([a-z0-9][a-z0-9_]*)\.sql$`)

var (
	// ErrUnknownMigration means the database records a migration this binary
	// does not contain, usually because the binary is older than the schema.
	ErrUnknownMigration = errors.New("database has a migration unknown to this binary")
	// ErrChecksumMismatch means an applied migration's file was changed.
	ErrChecksumMismatch = errors.New("applied migration does not match this binary")
	// ErrOutOfOrder means a pending migration is older than one already applied.
	ErrOutOfOrder = errors.New("pending migration is older than an applied migration")
	// ErrSchemaNotCurrent means migrations are pending.
	ErrSchemaNotCurrent = errors.New("database schema is not current")
)

// Migration is one versioned SQL file.
type Migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum string
}

func (m Migration) String() string { return fmt.Sprintf("%04d_%s", m.Version, m.Name) }

type appliedMigration struct {
	Version  int64
	Name     string
	Checksum string
}

// LoadMigrations reads NNNN_name.sql files from the root of fsys and returns
// them ordered by version. Any other entry is an error, so a misnamed file
// cannot be skipped silently.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var out []Migration
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() {
			return nil, fmt.Errorf("migration %s: directories are not allowed", e.Name())
		}
		match := migrationFileName.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("migration %s: name must match NNNN_lower_snake.sql", e.Name())
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %s: invalid version", e.Name())
		}
		if prev, ok := seen[version]; ok {
			return nil, fmt.Errorf("migration %s: version %d already used by %s", e.Name(), version, prev)
		}
		seen[version] = e.Name()

		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(raw)) == "" {
			return nil, fmt.Errorf("migration %s: empty", e.Name())
		}
		sum := sha256.Sum256(raw)
		out = append(out, Migration{
			Version:  version,
			Name:     match[2],
			SQL:      string(raw),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("no migrations found")
	}
	slices.SortFunc(out, func(a, b Migration) int {
		switch {
		case a.Version < b.Version:
			return -1
		case a.Version > b.Version:
			return 1
		}
		return 0
	})
	return out, nil
}

// plan checks applied against migrations and returns the pending ones in
// order. applied must be sorted by version.
func plan(applied []appliedMigration, migrations []Migration) ([]Migration, error) {
	byVersion := make(map[int64]Migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}

	done := make(map[int64]bool, len(applied))
	var maxApplied int64
	for _, a := range applied {
		m, ok := byVersion[a.Version]
		if !ok {
			return nil, fmt.Errorf("%w: version %d (%s)", ErrUnknownMigration, a.Version, a.Name)
		}
		if a.Checksum != m.Checksum || a.Name != m.Name {
			return nil, fmt.Errorf("%w: %s", ErrChecksumMismatch, m)
		}
		done[a.Version] = true
		maxApplied = max(maxApplied, a.Version)
	}

	var pending []Migration
	for _, m := range migrations {
		if done[m.Version] {
			continue
		}
		if m.Version < maxApplied {
			return nil, fmt.Errorf("%w: %s", ErrOutOfOrder, m)
		}
		pending = append(pending, m)
	}
	return pending, nil
}

// Migrate applies pending migrations in version order and returns how many it
// applied. Each migration runs in its own transaction together with its
// schema_migrations row, so a failed migration leaves no partial state and is
// retried on the next run. Running Migrate again after success applies
// nothing. Migration files must not contain transaction control statements.
func (db *DB) Migrate(ctx context.Context, migrations []Migration, log *slog.Logger) (int, error) {
	if db.closed.Load() {
		return 0, ErrClosed
	}
	if len(migrations) == 0 {
		return 0, errors.New("no migrations to apply")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	pooled, err := db.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire migration connection: %w", err)
	}
	// Closing the hijacked session releases the advisory lock even if the
	// unlock query below cannot run.
	conn := pooled.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return 0, fmt.Errorf("acquire migration lock: %w", err)
	}
	if _, err := conn.Exec(ctx, createMigrationsTable); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return 0, err
	}
	pending, err := plan(applied, migrations)
	if err != nil {
		return 0, err
	}

	for i, m := range pending {
		start := time.Now()
		if err := applyOne(ctx, conn, m); err != nil {
			return i, fmt.Errorf("apply migration %s: %w", m, err)
		}
		log.Info("migration applied", "migration", m.String(), "duration_ms", time.Since(start).Milliseconds())
	}
	return len(pending), nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			m.Version, m.Name, m.Checksum)
		return err
	})
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func loadApplied(ctx context.Context, q querier) ([]appliedMigration, error) {
	rows, err := q.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	applied, err := pgx.CollectRows(rows, pgx.RowToStructByPos[appliedMigration])
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// CheckSchema reports nil only if every migration is applied with a matching
// checksum and the database records no migration this binary lacks.
func (db *DB) CheckSchema(ctx context.Context, migrations []Migration) error {
	if db.closed.Load() {
		return ErrClosed
	}
	var exists bool
	if err := db.pool.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return fmt.Errorf("check schema_migrations: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: schema_migrations does not exist", ErrSchemaNotCurrent)
	}
	applied, err := loadApplied(ctx, db.pool)
	if err != nil {
		return err
	}
	pending, err := plan(applied, migrations)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: %d pending, first %s", ErrSchemaNotCurrent, len(pending), pending[0])
	}
	return nil
}
