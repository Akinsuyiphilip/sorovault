package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for golang-migrate

	sorovault "github.com/soroworks/sorovault"
)

// Migrate applies every pending migration to the database at databaseURL.
// It is safe to run against an already-current database, and safe to run
// concurrently: golang-migrate takes an advisory lock for the duration.
func Migrate(databaseURL string) error {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: applying migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back every migration, dropping the registry's tables.
// It exists for tests and local resets; it is not wired to a CLI command.
func MigrateDown(databaseURL string) error {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: reverting migrations: %w", err)
	}
	return nil
}

// MigrationVersion reports the applied version and whether the database is
// in a dirty state, meaning a previous migration failed partway and needs
// manual attention.
func MigrationVersion(databaseURL string) (version uint, dirty bool, err error) {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return 0, false, err
	}
	defer closeMigrator(m)

	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: reading migration version: %w", err)
	}
	return version, dirty, nil
}

func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(sorovault.Migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: reading embedded migrations: %w", err)
	}

	// The pgx stdlib driver registers itself as "pgx5"; golang-migrate's
	// postgres backend opens the URL through database/sql, and only
	// recognises a postgres:// or postgresql:// scheme.
	m, err := migrate.NewWithSourceInstance("iofs", src, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: preparing migrations: %w", err)
	}
	return m, nil
}

// MigrationStatus reports the migration version the database has applied,
// the latest version among the embedded migrations, and whether the database
// is marked dirty.
//
// It reads the version table directly rather than through golang-migrate,
// because the migrator creates that table when it opens the database: a
// check that only reads must not leave a table behind. A database with no
// version table has had no migration applied, which is version zero.
func MigrationStatus(ctx context.Context, databaseURL string) (current, latest uint, dirty bool, err error) {
	latest, err = LatestMigrationVersion()
	if err != nil {
		return 0, 0, false, err
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return 0, latest, false, fmt.Errorf("store: connecting to postgres: %w", err)
	}
	defer pool.Close()

	const q = `SELECT version, dirty FROM schema_migrations LIMIT 1`
	err = pool.QueryRow(ctx, q).Scan(&current, &dirty)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The table is there but empty: no migration has been applied.
		return 0, latest, false, nil
	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			// The table does not exist: no migration has been applied.
			return 0, latest, false, nil
		}
		return 0, latest, false, fmt.Errorf("store: reading migration version: %w", err)
	}
	return current, latest, dirty, nil
}

// LatestMigrationVersion reports the highest version among the embedded
// migrations.
//
// The versions are read through golang-migrate's own source, so the naming
// convention stays in one place.
func LatestMigrationVersion() (uint, error) {
	src, err := iofs.New(sorovault.Migrations, "migrations")
	if err != nil {
		return 0, fmt.Errorf("store: reading embedded migrations: %w", err)
	}

	version, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("store: reading embedded migrations: %w", err)
	}
	for {
		next, err := src.Next(version)
		if err != nil {
			// The source reports the end of the migrations as a missing
			// path.
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			return 0, fmt.Errorf("store: reading embedded migrations: %w", err)
		}
		version = next
	}
	return version, nil
}

// closeMigrator releases a migrator's source and database handles. Errors
// here cannot be acted on — the migration itself has already succeeded or
// failed — so they are deliberately dropped.
func closeMigrator(m *migrate.Migrate) {
	sourceErr, dbErr := m.Close()
	_, _ = sourceErr, dbErr
}

// ensure the postgres driver is linked in; golang-migrate resolves it by URL
// scheme at runtime rather than through a direct reference.
var _ = postgres.Postgres{}
