package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"madicloud/internal/application"
)

const (
	opCreateApplication = "applications.create"

	// idempotencyLockClass is the first key of the two-key advisory lock that
	// serializes requests sharing an idempotency key. The two-key form does
	// not collide with the single-key migration lock.
	idempotencyLockClass int32 = 0x6d616469 // "madi"

	// idempotencyPurgeBatch bounds how many expired keys one create removes.
	idempotencyPurgeBatch = 100
)

// PostgreSQL error codes and constraint names mapped to application errors.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"

	constraintAppName       = "applications_name_key"
	constraintAppNameFormat = "applications_name_format"
)

const appColumns = "id::text, name, desired_state, created_at, updated_at"

// ApplicationStore is the PostgreSQL application.Store.
type ApplicationStore struct {
	db *DB
}

var _ application.Store = (*ApplicationStore)(nil)

// NewApplicationStore returns a store using db.
func NewApplicationStore(db *DB) (*ApplicationStore, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	return &ApplicationStore{db: db}, nil
}

// snapshot is the JSON stored in idempotency_keys.result.
type snapshot struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	DesiredState string    `json:"desired_state"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func scanApp(row pgx.Row) (application.Application, error) {
	var a application.Application
	var state string
	if err := row.Scan(&a.ID, &a.Name, &state, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return application.Application{}, err
	}
	a.DesiredState = application.DesiredState(state)
	a.CreatedAt = a.CreatedAt.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()
	return a, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertApp(ctx context.Context, q queryRower, name string) (application.Application, error) {
	return scanApp(q.QueryRow(ctx,
		"INSERT INTO applications (name) VALUES ($1) RETURNING "+appColumns, name))
}

// CreateApplication inserts an application. With idem, it runs in one
// transaction holding a per-key advisory lock, so concurrent requests with the
// same key execute one at a time: the first creates, the rest replay.
func (s *ApplicationStore) CreateApplication(ctx context.Context, name string, idem *application.Idempotency) (application.Application, bool, error) {
	if s.db.closed.Load() {
		return application.Application{}, false, mapError(ErrClosed)
	}
	if idem == nil {
		app, err := insertApp(ctx, s.db.pool, name)
		return app, false, mapError(err)
	}

	var app application.Application
	var replayed bool
	err := pgx.BeginFunc(ctx, s.db.pool, func(tx pgx.Tx) error {
		app, replayed = application.Application{}, false

		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
			idempotencyLockClass, lockKey(opCreateApplication, idem.Key)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys
			WHERE (operation, key) IN (
				SELECT operation, key FROM idempotency_keys
				WHERE expires_at <= now()
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)`, idempotencyPurgeBatch); err != nil {
			return err
		}

		var hash string
		var raw []byte
		err := tx.QueryRow(ctx, `
			SELECT request_hash, result FROM idempotency_keys
			WHERE operation = $1 AND key = $2 AND expires_at > now()`,
			opCreateApplication, idem.Key).Scan(&hash, &raw)
		switch {
		case err == nil:
			if hash != idem.RequestHash {
				return application.ErrIdempotencyKeyReused
			}
			var snap snapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				return fmt.Errorf("decode idempotency result: %w", err)
			}
			app = application.Application{
				ID:           snap.ID,
				Name:         snap.Name,
				DesiredState: application.DesiredState(snap.DesiredState),
				CreatedAt:    snap.CreatedAt.UTC(),
				UpdatedAt:    snap.UpdatedAt.UTC(),
			}
			replayed = true
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		// Any remaining row for this key is expired.
		if _, err := tx.Exec(ctx, "DELETE FROM idempotency_keys WHERE operation = $1 AND key = $2",
			opCreateApplication, idem.Key); err != nil {
			return err
		}

		created, err := insertApp(ctx, tx, name)
		if err != nil {
			return err
		}
		raw, err = json.Marshal(snapshot{
			ID:           created.ID,
			Name:         created.Name,
			DesiredState: string(created.DesiredState),
			CreatedAt:    created.CreatedAt,
			UpdatedAt:    created.UpdatedAt,
		})
		if err != nil {
			return fmt.Errorf("encode idempotency result: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (operation, key, request_hash, result, expires_at)
			VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`,
			opCreateApplication, idem.Key, idem.RequestHash, raw, idem.TTL.Seconds()); err != nil {
			return err
		}
		app = created
		return nil
	})
	if err != nil {
		return application.Application{}, false, mapError(err)
	}
	return app, replayed, nil
}

// GetApplication returns the application with id. id must be a canonical UUID.
func (s *ApplicationStore) GetApplication(ctx context.Context, id string) (application.Application, error) {
	if s.db.closed.Load() {
		return application.Application{}, mapError(ErrClosed)
	}
	app, err := scanApp(s.db.pool.QueryRow(ctx,
		"SELECT "+appColumns+" FROM applications WHERE id = $1", id))
	return app, mapError(err)
}

// GetApplicationByName returns the application named name.
func (s *ApplicationStore) GetApplicationByName(ctx context.Context, name string) (application.Application, error) {
	if s.db.closed.Load() {
		return application.Application{}, mapError(ErrClosed)
	}
	app, err := scanApp(s.db.pool.QueryRow(ctx,
		"SELECT "+appColumns+" FROM applications WHERE name = $1", name))
	return app, mapError(err)
}

// ListApplications returns up to limit applications named after after, in
// byte order of name.
func (s *ApplicationStore) ListApplications(ctx context.Context, after string, limit int) ([]application.Application, error) {
	if s.db.closed.Load() {
		return nil, mapError(ErrClosed)
	}
	rows, err := s.db.pool.Query(ctx,
		"SELECT "+appColumns+" FROM applications WHERE name > $1 ORDER BY name LIMIT $2", after, limit)
	if err != nil {
		return nil, mapError(err)
	}
	apps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (application.Application, error) {
		return scanApp(r)
	})
	if err != nil {
		return nil, mapError(err)
	}
	return apps, nil
}

// DeleteApplication removes the application. A foreign key from a dependent
// resource makes the delete fail atomically with ErrHasDependents, so the
// dependency check cannot race with a concurrent insert of a dependent.
func (s *ApplicationStore) DeleteApplication(ctx context.Context, id string) (application.Application, error) {
	if s.db.closed.Load() {
		return application.Application{}, mapError(ErrClosed)
	}
	app, err := scanApp(s.db.pool.QueryRow(ctx,
		"DELETE FROM applications WHERE id = $1 RETURNING "+appColumns, id))
	return app, mapError(err)
}

func lockKey(operation, key string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(operation))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return int32(h.Sum32())
}

// mapError converts driver errors to application errors. Unmapped errors keep
// their detail for server logs; the API layer never returns them to clients.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{
		application.ErrNotFound,
		application.ErrNameTaken,
		application.ErrHasDependents,
		application.ErrIdempotencyKeyReused,
		application.ErrUnavailable,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == constraintAppName:
			return application.ErrNameTaken
		case pgErr.Code == pgForeignKeyViolation:
			return application.ErrHasDependents
		case pgErr.Code == pgCheckViolation && pgErr.ConstraintName == constraintAppNameFormat:
			return &application.ValidationError{Field: "name", Message: "name is invalid"}
		}
		return fmt.Errorf("postgres %s: %w", pgErr.Code, err)
	}

	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) || errors.Is(err, ErrClosed) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", application.ErrUnavailable, err)
	}
	return err
}
