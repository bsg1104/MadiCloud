package database

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"madicloud/internal/database/databasetest"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), databasetest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustLoad(t *testing.T, files map[string]string) []Migration {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, sql := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(sql)}
	}
	set, err := LoadMigrations(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func tableExists(t *testing.T, db *DB, name string) bool {
	t.Helper()
	var ok bool
	if err := db.pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func appliedVersions(t *testing.T, db *DB) []int64 {
	t.Helper()
	rows, err := loadApplied(context.Background(), db.pool)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, r := range rows {
		out = append(out, r.Version)
	}
	return out
}

func TestIntegrationMigrateAndReady(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	set := mustLoad(t, map[string]string{
		"0001_one.sql": "CREATE TABLE t_one (id int PRIMARY KEY);\nINSERT INTO t_one VALUES (1);",
		"0002_two.sql": "CREATE TABLE t_two (id int);",
	})
	ready, err := NewReadiness(db, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := ready.CheckReady(ctx); !errors.Is(err, ErrSchemaNotCurrent) {
		t.Fatalf("CheckReady before migrate = %v, want ErrSchemaNotCurrent", err)
	}

	n, err := db.Migrate(ctx, set[:1], testLog())
	if err != nil || n != 1 {
		t.Fatalf("Migrate first = %d, %v", n, err)
	}
	if err := ready.CheckReady(ctx); !errors.Is(err, ErrSchemaNotCurrent) {
		t.Fatalf("CheckReady with one pending = %v, want ErrSchemaNotCurrent", err)
	}

	n, err = db.Migrate(ctx, set, testLog())
	if err != nil || n != 1 {
		t.Fatalf("Migrate rest = %d, %v", n, err)
	}
	if err := ready.CheckReady(ctx); err != nil {
		t.Fatalf("CheckReady after migrate: %v", err)
	}

	n, err = db.Migrate(ctx, set, testLog())
	if err != nil || n != 0 {
		t.Fatalf("Migrate repeat = %d, %v, want 0, nil", n, err)
	}
	var rows int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM t_one").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("t_one rows = %d, %v; repeat run must not re-execute SQL", rows, err)
	}
	if got := appliedVersions(t, db); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("applied = %v", got)
	}
}

func TestIntegrationConcurrentMigrate(t *testing.T) {
	t.Parallel()
	cfg := databasetest.New(t)
	set := mustLoad(t, map[string]string{
		"0001_one.sql": "CREATE TABLE t_one (id int); SELECT pg_sleep(0.2);",
		"0002_two.sql": "CREATE TABLE t_two (id int);",
	})

	const workers = 4
	var wg sync.WaitGroup
	counts := make([]int, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(context.Background(), cfg)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			counts[i], errs[i] = db.Migrate(context.Background(), set, testLog())
		}()
	}
	wg.Wait()

	total := 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		total += counts[i]
	}
	if total != len(set) {
		t.Fatalf("applied %d migrations across workers, want %d", total, len(set))
	}
}

func TestIntegrationFailedMigrationRollsBack(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()

	broken := mustLoad(t, map[string]string{
		"0001_ok.sql":     "CREATE TABLE t_ok (id int);",
		"0002_broken.sql": "CREATE TABLE t_partial (id int);\nSELECT 1/0;",
	})
	n, err := db.Migrate(ctx, broken, testLog())
	if err == nil {
		t.Fatal("Migrate succeeded with a failing migration")
	}
	if n != 1 || !strings.Contains(err.Error(), "0002_broken") || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("Migrate = %d, %v", n, err)
	}
	if !tableExists(t, db, "t_ok") {
		t.Fatal("0001 was rolled back")
	}
	if tableExists(t, db, "t_partial") {
		t.Fatal("failed migration left t_partial behind")
	}
	if got := appliedVersions(t, db); len(got) != 1 || got[0] != 1 {
		t.Fatalf("applied = %v, want [1]", got)
	}
	if err := db.CheckSchema(ctx, broken); !errors.Is(err, ErrSchemaNotCurrent) {
		t.Fatalf("CheckSchema = %v, want ErrSchemaNotCurrent", err)
	}

	fixed := mustLoad(t, map[string]string{
		"0001_ok.sql":     "CREATE TABLE t_ok (id int);",
		"0002_broken.sql": "CREATE TABLE t_partial (id int);",
	})
	if n, err := db.Migrate(ctx, fixed, testLog()); err != nil || n != 1 {
		t.Fatalf("Migrate after fix = %d, %v", n, err)
	}
	if err := db.CheckSchema(ctx, fixed); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationDetectsDrift(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	ctx := context.Background()

	two := mustLoad(t, map[string]string{
		"0001_one.sql": "CREATE TABLE t_one (id int);",
		"0002_two.sql": "CREATE TABLE t_two (id int);",
	})
	if _, err := db.Migrate(ctx, two, testLog()); err != nil {
		t.Fatal(err)
	}

	edited := mustLoad(t, map[string]string{
		"0001_one.sql": "CREATE TABLE t_one (id bigint);",
		"0002_two.sql": "CREATE TABLE t_two (id int);",
	})
	if _, err := db.Migrate(ctx, edited, testLog()); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Migrate edited = %v, want ErrChecksumMismatch", err)
	}
	if err := db.CheckSchema(ctx, edited); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("CheckSchema edited = %v, want ErrChecksumMismatch", err)
	}

	older := two[:1]
	if _, err := db.Migrate(ctx, older, testLog()); !errors.Is(err, ErrUnknownMigration) {
		t.Fatalf("Migrate older binary = %v, want ErrUnknownMigration", err)
	}
	if err := db.CheckSchema(ctx, older); !errors.Is(err, ErrUnknownMigration) {
		t.Fatalf("CheckSchema older binary = %v, want ErrUnknownMigration", err)
	}
}

func TestIntegrationWrongPasswordNotLeaked(t *testing.T) {
	t.Parallel()
	cfg := databasetest.New(t)
	cfg.Password = "wrong-" + secretPassword

	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = db.Ping(ctx)
	if err == nil {
		t.Fatal("Ping succeeded with the wrong password")
	}
	if strings.Contains(err.Error(), secretPassword) {
		t.Fatalf("error leaked password: %v", err)
	}
}

func TestIntegrationCloseWithOpenConnections(t *testing.T) {
	t.Parallel()
	cfg := databasetest.New(t)
	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := db.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if db.pool.Stat().TotalConns() == 0 {
		t.Fatal("expected an idle connection before Close")
	}

	done := make(chan struct{})
	go func() {
		db.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	if got := db.pool.Stat().TotalConns(); got != 0 {
		t.Fatalf("connections after Close = %d", got)
	}
}
