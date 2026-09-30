package database

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"madicloud/internal/application"
	"madicloud/internal/config"
)

const secretPassword = "pw-2c9e-must-not-leak"

// unreachableConfig points at a loopback port with nothing listening.
func unreachableConfig(t *testing.T) config.Database {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return config.Database{
		Host:     "127.0.0.1",
		Port:     port,
		Name:     "madicloud",
		User:     "madicloud",
		Password: secretPassword,
		SSLMode:  "disable",
	}
}

func TestOpenDoesNotConnect(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), unreachableConfig(t))
	if err != nil {
		t.Fatalf("Open with PostgreSQL down: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = db.Ping(ctx)
	if err == nil {
		t.Fatal("Ping succeeded with nothing listening")
	}
	if strings.Contains(err.Error(), secretPassword) {
		t.Fatalf("Ping error leaked password: %v", err)
	}
}

func TestReadinessUnavailable(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), unreachableConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r, err := NewReadiness(db, []Migration{{Version: 1, Name: "x", SQL: "SELECT 1", Checksum: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = r.CheckReady(ctx)
	if err == nil {
		t.Fatal("CheckReady succeeded with nothing listening")
	}
	if strings.Contains(err.Error(), secretPassword) {
		t.Fatalf("readiness error leaked password: %v", err)
	}
}

func TestNewReadinessValidates(t *testing.T) {
	t.Parallel()

	if _, err := NewReadiness(nil, []Migration{{Version: 1}}); err == nil {
		t.Fatal("expected error for nil db")
	}
	db, err := Open(context.Background(), unreachableConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := NewReadiness(db, nil); err == nil {
		t.Fatal("expected error for no migrations")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), unreachableConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db.Close()

	ctx := context.Background()
	if err := db.Ping(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping after Close = %v, want ErrClosed", err)
	}
	set := []Migration{{Version: 1, Name: "x", SQL: "SELECT 1", Checksum: "c"}}
	if err := db.CheckSchema(ctx, set); !errors.Is(err, ErrClosed) {
		t.Fatalf("CheckSchema after Close = %v, want ErrClosed", err)
	}
	if _, err := db.Migrate(ctx, set, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Migrate after Close = %v, want ErrClosed", err)
	}
}

// Not parallel: it compares process-wide goroutine counts.
func TestCloseStopsPoolGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	db, err := Open(context.Background(), unreachableConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = db.Ping(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		db.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: before=%d after=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApplicationStoreUnavailable(t *testing.T) {
	t.Parallel()

	db, err := Open(context.Background(), unreachableConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewApplicationStore(db)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	id := "00000000-0000-4000-8000-000000000000"
	checks := map[string]error{}
	_, _, checks["create"] = store.CreateApplication(ctx, "down-app", nil)
	_, _, checks["create idem"] = store.CreateApplication(ctx, "down-app", &application.Idempotency{Key: "k", RequestHash: "h", TTL: time.Hour})
	_, checks["get"] = store.GetApplication(ctx, id)
	_, checks["get by name"] = store.GetApplicationByName(ctx, "down-app")
	_, checks["list"] = store.ListApplications(ctx, "", 10)
	_, checks["delete"] = store.DeleteApplication(ctx, id)
	for op, err := range checks {
		if !errors.Is(err, application.ErrUnavailable) {
			t.Errorf("%s = %v, want ErrUnavailable", op, err)
		}
		if err != nil && strings.Contains(err.Error(), secretPassword) {
			t.Errorf("%s leaked password", op)
		}
	}

	if _, err := NewApplicationStore(nil); err == nil {
		t.Fatal("NewApplicationStore(nil) succeeded")
	}
}

func TestConnURLEscapesCredentials(t *testing.T) {
	t.Parallel()

	u := connURL(config.Database{
		Host: "::1", Port: 5432, Name: "db", User: "u@x", Password: "p:/@?#", SSLMode: "disable",
	})
	for _, want := range []string{"postgres://u%40x:p%3A%2F%40%3F%23@[::1]:5432/db?", "sslmode=disable", "connect_timeout=5", "application_name=madicloudd"} {
		if !strings.Contains(u, want) {
			t.Errorf("connURL = %q, missing %q", u, want)
		}
	}
}

func TestLoadMigrations(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"0002_second.sql": {Data: []byte("SELECT 2;")},
		"0001_first.sql":  {Data: []byte("SELECT 1;")},
		"0010_tenth.sql":  {Data: []byte("SELECT 10;")},
	}
	set, err := LoadMigrations(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range set {
		got = append(got, m.String())
		if len(m.Checksum) != 64 {
			t.Errorf("%s checksum = %q", m, m.Checksum)
		}
	}
	if strings.Join(got, ",") != "0001_first,0002_second,0010_tenth" {
		t.Fatalf("order = %v", got)
	}

	again, err := LoadMigrations(fsys)
	if err != nil {
		t.Fatal(err)
	}
	for i := range set {
		if set[i].Checksum != again[i].Checksum {
			t.Fatalf("checksum not deterministic for %s", set[i])
		}
	}
}

func TestLoadMigrationsRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fsys    fstest.MapFS
		wantErr string
	}{
		{name: "empty set", fsys: fstest.MapFS{}, wantErr: "no migrations found"},
		{name: "bad name", fsys: fstest.MapFS{"1_x.sql": {Data: []byte("SELECT 1")}}, wantErr: "name must match"},
		{name: "upper case", fsys: fstest.MapFS{"0001_Init.sql": {Data: []byte("SELECT 1")}}, wantErr: "name must match"},
		{name: "not sql", fsys: fstest.MapFS{"README.md": {Data: []byte("x")}}, wantErr: "name must match"},
		{name: "version zero", fsys: fstest.MapFS{"0000_zero.sql": {Data: []byte("SELECT 1")}}, wantErr: "invalid version"},
		{name: "blank file", fsys: fstest.MapFS{"0001_blank.sql": {Data: []byte(" \n\t")}}, wantErr: "empty"},
		{name: "directory", fsys: fstest.MapFS{"sub/0001_x.sql": {Data: []byte("SELECT 1")}}, wantErr: "directories are not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadMigrations(tt.fsys)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadMigrationsDuplicateVersion(t *testing.T) {
	t.Parallel()

	_, err := LoadMigrations(fstest.MapFS{
		"0001_a.sql":  {Data: []byte("SELECT 1")},
		"00001_b.sql": {Data: []byte("SELECT 1")},
	})
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlan(t *testing.T) {
	t.Parallel()

	m1 := Migration{Version: 1, Name: "one", Checksum: "c1"}
	m2 := Migration{Version: 2, Name: "two", Checksum: "c2"}
	m3 := Migration{Version: 3, Name: "three", Checksum: "c3"}
	set := []Migration{m1, m2, m3}
	a := func(m Migration) appliedMigration {
		return appliedMigration{Version: m.Version, Name: m.Name, Checksum: m.Checksum}
	}

	tests := []struct {
		name    string
		applied []appliedMigration
		want    []int64
		wantErr error
	}{
		{name: "fresh", want: []int64{1, 2, 3}},
		{name: "partial", applied: []appliedMigration{a(m1)}, want: []int64{2, 3}},
		{name: "current", applied: []appliedMigration{a(m1), a(m2), a(m3)}},
		{name: "unknown", applied: []appliedMigration{a(m1), {Version: 9, Name: "nine", Checksum: "c9"}}, wantErr: ErrUnknownMigration},
		{name: "checksum changed", applied: []appliedMigration{{Version: 1, Name: "one", Checksum: "other"}}, wantErr: ErrChecksumMismatch},
		{name: "renamed", applied: []appliedMigration{{Version: 1, Name: "uno", Checksum: "c1"}}, wantErr: ErrChecksumMismatch},
		{name: "gap", applied: []appliedMigration{a(m1), a(m3)}, wantErr: ErrOutOfOrder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pending, err := plan(tt.applied, set)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			for _, m := range pending {
				got = append(got, m.Version)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("pending = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("pending = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
