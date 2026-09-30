package database

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"madicloud/internal/application"
	"madicloud/internal/database/databasetest"
	"madicloud/migrations"
)

func openMigrated(t *testing.T) (*DB, *ApplicationStore) {
	t.Helper()
	db := openTestDB(t)
	set, err := LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(context.Background(), set, testLog()); err != nil {
		t.Fatal(err)
	}
	store, err := NewApplicationStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, store
}

func idem(key, name string) *application.Idempotency {
	return &application.Idempotency{Key: key, RequestHash: "hash-" + name, TTL: time.Hour}
}

func TestIntegrationApplicationCRUD(t *testing.T) {
	t.Parallel()
	_, store := openMigrated(t)
	ctx := context.Background()

	app, replayed, err := store.CreateApplication(ctx, "my-app", nil)
	if err != nil || replayed {
		t.Fatalf("create = %+v, %v, %v", app, replayed, err)
	}
	if _, err := application.ParseID(app.ID); err != nil {
		t.Fatalf("id %q is not a canonical UUID", app.ID)
	}
	if app.Name != "my-app" || app.DesiredState != application.DesiredActive {
		t.Fatalf("app = %+v", app)
	}
	if !app.CreatedAt.Equal(app.UpdatedAt) || app.CreatedAt.Location() != time.UTC || time.Since(app.CreatedAt) > time.Minute {
		t.Fatalf("timestamps = %v / %v", app.CreatedAt, app.UpdatedAt)
	}

	got, err := store.GetApplication(ctx, app.ID)
	if err != nil || got != app {
		t.Fatalf("get = %+v, %v, want %+v", got, err, app)
	}
	byName, err := store.GetApplicationByName(ctx, "my-app")
	if err != nil || byName != app {
		t.Fatalf("get by name = %+v, %v", byName, err)
	}

	list, err := store.ListApplications(ctx, "", 10)
	if err != nil || len(list) != 1 || list[0] != app {
		t.Fatalf("list = %+v, %v", list, err)
	}

	deleted, err := store.DeleteApplication(ctx, app.ID)
	if err != nil || deleted != app {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	if _, err := store.GetApplication(ctx, app.ID); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
	if _, err := store.DeleteApplication(ctx, app.ID); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if _, err := store.GetApplicationByName(ctx, "my-app"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("get by name after delete = %v", err)
	}
	if _, _, err := store.CreateApplication(ctx, "my-app", nil); err != nil {
		t.Fatalf("name not reusable after delete: %v", err)
	}
}

func TestIntegrationApplicationDuplicateName(t *testing.T) {
	t.Parallel()
	_, store := openMigrated(t)
	ctx := context.Background()

	if _, _, err := store.CreateApplication(ctx, "dup-app", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateApplication(ctx, "dup-app", nil); !errors.Is(err, application.ErrNameTaken) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, _, err := store.CreateApplication(ctx, "dup-app", idem("k", "dup-app")); !errors.Is(err, application.ErrNameTaken) {
		t.Fatalf("duplicate with key = %v", err)
	}
}

func TestIntegrationApplicationConcurrentDuplicate(t *testing.T) {
	t.Parallel()
	_, store := openMigrated(t)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = store.CreateApplication(context.Background(), "race-app", nil)
		}()
	}
	wg.Wait()

	ok, taken := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, application.ErrNameTaken):
			taken++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || taken != n-1 {
		t.Fatalf("created=%d taken=%d", ok, taken)
	}
}

func TestIntegrationApplicationListOrder(t *testing.T) {
	t.Parallel()
	_, store := openMigrated(t)
	ctx := context.Background()

	// Byte order: '-' (0x2d) < digits < letters. Locale collations would
	// order these differently.
	for _, n := range []string{"zed", "a-b", "ab1", "abc", "a1b", "b-a"} {
		if _, _, err := store.CreateApplication(ctx, n, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := "a-b,a1b,ab1,abc,b-a,zed"

	var got []string
	after := ""
	for {
		page, err := store.ListApplications(ctx, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			got = append(got, a.Name)
		}
		after = page[len(page)-1].Name
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

func TestIntegrationIdempotentCreate(t *testing.T) {
	t.Parallel()
	db, store := openMigrated(t)
	ctx := context.Background()

	first, replayed, err := store.CreateApplication(ctx, "idem-app", idem("key-1", "idem-app"))
	if err != nil || replayed {
		t.Fatalf("first = %v, %v", replayed, err)
	}
	for range 3 {
		again, replayed, err := store.CreateApplication(ctx, "idem-app", idem("key-1", "idem-app"))
		if err != nil || !replayed || again != first {
			t.Fatalf("replay = %+v %v %v, want %+v", again, replayed, err, first)
		}
	}
	if _, _, err := store.CreateApplication(ctx, "other-app", idem("key-1", "other-app")); !errors.Is(err, application.ErrIdempotencyKeyReused) {
		t.Fatalf("same key, different request = %v", err)
	}

	// The replay returns the original result even after the application is
	// deleted, and does not recreate it.
	if _, err := store.DeleteApplication(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	again, replayed, err := store.CreateApplication(ctx, "idem-app", idem("key-1", "idem-app"))
	if err != nil || !replayed || again != first {
		t.Fatalf("replay after delete = %+v %v %v", again, replayed, err)
	}
	if _, err := store.GetApplication(ctx, first.ID); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("replay recreated the application")
	}

	var rows int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("idempotency rows = %d, %v", rows, err)
	}
}

func TestIntegrationIdempotentConcurrent(t *testing.T) {
	t.Parallel()
	_, store := openMigrated(t)

	const n = 12
	var wg sync.WaitGroup
	apps := make([]application.Application, n)
	replays := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			apps[i], replays[i], errs[i] = store.CreateApplication(context.Background(), "burst-app", idem("burst", "burst-app"))
		}()
	}
	close(start)
	wg.Wait()

	fresh := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if apps[i] != apps[0] {
			t.Fatalf("request %d got %+v, want %+v", i, apps[i], apps[0])
		}
		if !replays[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d requests created an application, want 1", fresh)
	}
	list, err := store.ListApplications(context.Background(), "", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("applications = %d, %v", len(list), err)
	}
}

func TestIntegrationIdempotentFailureNotRecorded(t *testing.T) {
	t.Parallel()
	db, store := openMigrated(t)
	ctx := context.Background()

	blocker, _, err := store.CreateApplication(ctx, "taken-app", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateApplication(ctx, "taken-app", idem("retry-key", "taken-app")); !errors.Is(err, application.ErrNameTaken) {
		t.Fatalf("create over taken name = %v", err)
	}
	var rows int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys WHERE key = 'retry-key'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed request left %d key rows, %v", rows, err)
	}

	if _, err := store.DeleteApplication(ctx, blocker.ID); err != nil {
		t.Fatal(err)
	}
	app, replayed, err := store.CreateApplication(ctx, "taken-app", idem("retry-key", "taken-app"))
	if err != nil || replayed || app.ID == blocker.ID {
		t.Fatalf("retry = %+v %v %v", app, replayed, err)
	}
}

func TestIntegrationIdempotencyExpiry(t *testing.T) {
	t.Parallel()
	db, store := openMigrated(t)
	ctx := context.Background()

	if _, _, err := store.CreateApplication(ctx, "old-app", idem("old-key", "old-app")); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"stale-1", "stale-2"} {
		if _, _, err := store.CreateApplication(ctx, "app-"+k, idem(k, k)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET created_at = now() - interval '2 days', expires_at = now() - interval '1 day'`); err != nil {
		t.Fatal(err)
	}

	app, replayed, err := store.CreateApplication(ctx, "new-app", idem("old-key", "new-app"))
	if err != nil || replayed || app.Name != "new-app" {
		t.Fatalf("expired key reuse = %+v %v %v", app, replayed, err)
	}

	var stale int
	if err := db.pool.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys WHERE expires_at <= now()").Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("expired rows left = %d, %v", stale, err)
	}
}

func TestIntegrationApplicationConstraints(t *testing.T) {
	t.Parallel()
	db, store := openMigrated(t)
	ctx := context.Background()

	if _, _, err := store.CreateApplication(ctx, "fine-app", nil); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		sql        string
		code       string
		constraint string
	}{
		{"uppercase", "INSERT INTO applications (name) VALUES ('Bad-App')", pgCheckViolation, "applications_name_format"},
		{"too short", "INSERT INTO applications (name) VALUES ('ab')", pgCheckViolation, "applications_name_format"},
		{"too long", "INSERT INTO applications (name) VALUES (repeat('a', 64))", pgCheckViolation, "applications_name_format"},
		{"double hyphen", "INSERT INTO applications (name) VALUES ('a--b')", pgCheckViolation, "applications_name_format"},
		{"trailing hyphen", "INSERT INTO applications (name) VALUES ('abc-')", pgCheckViolation, "applications_name_format"},
		{"leading digit", "INSERT INTO applications (name) VALUES ('1abc')", pgCheckViolation, "applications_name_format"},
		{"null name", "INSERT INTO applications (name) VALUES (NULL)", "23502", ""},
		{"bad state", "INSERT INTO applications (name, desired_state) VALUES ('state-app', 'running')", pgCheckViolation, "applications_desired_state_valid"},
		{"null state", "INSERT INTO applications (name, desired_state) VALUES ('state-app', NULL)", "23502", ""},
		{"duplicate", "INSERT INTO applications (name) VALUES ('fine-app')", pgUniqueViolation, "applications_name_key"},
		{"updated before created", "INSERT INTO applications (name, created_at, updated_at) VALUES ('time-app', now(), now() - interval '1 second')", pgCheckViolation, "applications_updated_after_created"},
		{"empty idempotency key", "INSERT INTO idempotency_keys (operation, key, request_hash, result, expires_at) VALUES ('x', '', 'h', '{}', now() + interval '1 hour')", pgCheckViolation, "idempotency_keys_key_length"},
	}
	for _, tt := range tests {
		_, err := db.pool.Exec(ctx, tt.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != tt.code || (tt.constraint != "" && pgErr.ConstraintName != tt.constraint) {
			t.Errorf("%s: err = %v, want %s %s", tt.name, err, tt.code, tt.constraint)
		}
	}

	// 'deleted' is a valid desired state even though this phase never writes it.
	if _, err := db.pool.Exec(ctx, "INSERT INTO applications (name, desired_state) VALUES ('deleted-app', 'deleted')"); err != nil {
		t.Fatalf("deleted state rejected: %v", err)
	}

	// Constraint errors map to application errors, not raw driver errors.
	_, err := insertApp(ctx, db.pool, "Bad-App")
	var ve *application.ValidationError
	if !errors.As(mapError(err), &ve) {
		t.Fatalf("check violation mapped to %v", mapError(err))
	}
}

func TestIntegrationDeleteWithDependents(t *testing.T) {
	t.Parallel()
	db, store := openMigrated(t)
	ctx := context.Background()

	app, _, err := store.CreateApplication(ctx, "parent-app", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Stand-in for a future dependent resource, created only in this test
	// database.
	if _, err := db.pool.Exec(ctx, `
		CREATE TABLE test_dependents (application_id uuid NOT NULL REFERENCES applications(id));
		INSERT INTO test_dependents VALUES ('`+app.ID+`');`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteApplication(ctx, app.ID); !errors.Is(err, application.ErrHasDependents) {
		t.Fatalf("delete with dependent = %v", err)
	}
	if _, err := store.GetApplication(ctx, app.ID); err != nil {
		t.Fatalf("application removed despite dependent: %v", err)
	}
}

func TestIntegrationApplicationStoreClosed(t *testing.T) {
	t.Parallel()
	db, store := openMigrated(t)
	db.Close()
	if _, _, err := store.CreateApplication(context.Background(), "late-app", nil); !errors.Is(err, application.ErrUnavailable) {
		t.Fatalf("create on closed db = %v", err)
	}
}

func TestIntegrationApplicationsMigrationUpgrade(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), databasetest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	set, err := LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.Migrate(ctx, set[:1], testLog()); err != nil || n != 1 {
		t.Fatalf("phase 1 schema = %d, %v", n, err)
	}
	if err := db.CheckSchema(ctx, set); !errors.Is(err, ErrSchemaNotCurrent) {
		t.Fatalf("CheckSchema with 0002 pending = %v", err)
	}
	if n, err := db.Migrate(ctx, set, testLog()); err != nil || n != len(set)-1 {
		t.Fatalf("upgrade = %d, %v", n, err)
	}
	if err := db.CheckSchema(ctx, set); err != nil {
		t.Fatal(err)
	}
}
