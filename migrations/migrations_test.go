package migrations_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"madicloud/internal/database"
	"madicloud/internal/database/databasetest"
	"madicloud/migrations"
)

// Checksums of applied migrations. An applied migration must never change;
// if this test fails, add a new migration instead of editing an old one.
var appliedChecksums = map[string]string{
	"0001_cluster_identity": "5f44df9c7525",
}

func TestEmbeddedMigrationsLoad(t *testing.T) {
	t.Parallel()

	set, err := database.LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	for i, m := range set {
		if m.Version != int64(i+1) {
			t.Fatalf("migration %d has version %d; versions must be contiguous from 1", i, m.Version)
		}
	}
	want := []string{"0001_cluster_identity", "0002_applications"}
	if len(set) != len(want) {
		t.Fatalf("migrations = %v, want %v", set, want)
	}
	for i := range want {
		if set[i].String() != want[i] {
			t.Fatalf("migration %d = %s, want %s", i, set[i], want[i])
		}
	}
}

func TestAppliedMigrationsUnchanged(t *testing.T) {
	t.Parallel()

	set, err := database.LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range set {
		prefix, ok := appliedChecksums[m.String()]
		if !ok {
			continue
		}
		if m.Checksum[:len(prefix)] != prefix {
			t.Errorf("%s checksum changed: %s, recorded prefix %s", m, m.Checksum, prefix)
		}
	}
}

func TestIntegrationEmbeddedMigrations(t *testing.T) {
	t.Parallel()
	cfg := databasetest.New(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	set, err := database.LoadMigrations(migrations.FS())
	if err != nil {
		t.Fatal(err)
	}

	if n, err := db.Migrate(ctx, set, log); err != nil || n != len(set) {
		t.Fatalf("clean migrate = %d, %v", n, err)
	}
	if n, err := db.Migrate(ctx, set, log); err != nil || n != 0 {
		t.Fatalf("second migrate = %d, %v", n, err)
	}
	if err := db.CheckSchema(ctx, set); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}

	edited := append([]database.Migration(nil), set...)
	last := edited[len(edited)-1]
	last.SQL += "\n-- edited\n"
	last.Checksum = "0000000000000000000000000000000000000000000000000000000000000000"
	edited[len(edited)-1] = last
	if _, err := db.Migrate(ctx, edited, log); !errors.Is(err, database.ErrChecksumMismatch) {
		t.Fatalf("edited migration = %v, want ErrChecksumMismatch", err)
	}
	if err := db.CheckSchema(ctx, edited); !errors.Is(err, database.ErrChecksumMismatch) {
		t.Fatalf("CheckSchema edited = %v", err)
	}

	older := set[:len(set)-1]
	if _, err := db.Migrate(ctx, older, log); !errors.Is(err, database.ErrUnknownMigration) {
		t.Fatalf("older binary = %v, want ErrUnknownMigration", err)
	}
	if err := db.CheckSchema(ctx, older); !errors.Is(err, database.ErrUnknownMigration) {
		t.Fatalf("CheckSchema older = %v", err)
	}
}
