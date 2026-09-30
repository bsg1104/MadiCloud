package database

import (
	"context"
	"errors"
)

// Readiness reports whether the control plane can use its database: PostgreSQL
// answers and the schema matches the migrations compiled into this binary.
type Readiness struct {
	db         *DB
	migrations []Migration
}

// NewReadiness returns a readiness check for db against migrations.
func NewReadiness(db *DB, migrations []Migration) (*Readiness, error) {
	if db == nil {
		return nil, errors.New("database is required")
	}
	if len(migrations) == 0 {
		return nil, errors.New("migrations are required")
	}
	return &Readiness{db: db, migrations: migrations}, nil
}

// CheckReady returns nil if the database is reachable and migrated.
func (r *Readiness) CheckReady(ctx context.Context) error {
	if err := r.db.Ping(ctx); err != nil {
		return err
	}
	return r.db.CheckSchema(ctx, r.migrations)
}
