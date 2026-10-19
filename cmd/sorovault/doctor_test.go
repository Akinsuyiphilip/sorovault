package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/config"
	"github.com/soroworks/sorovault/internal/stellar"
)

// healthyDoctorDeps returns dependencies whose every check passes.
func healthyDoctorDeps() doctorDeps {
	return doctorDeps{
		Ping: func(context.Context) error { return nil },
		Migrations: func(context.Context) (uint, uint, bool, error) {
			return 2, 2, false, nil
		},
		Passphrase:       func(context.Context) (string, error) { return stellar.NetworkTestnet, nil },
		ExpectPassphrase: stellar.NetworkTestnet,
	}
}

// runDoctorCmd runs the doctor command against the given dependencies and
// returns what it printed and the error it ended with.
func runDoctorCmd(t *testing.T, deps doctorDeps, args ...string) (string, error) {
	t.Helper()

	cmd := newDoctorCmd(func() (doctorDeps, error) { return deps, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return out.String(), err
}

// TestDoctorChecksTheFourThings proves the command reports everything the
// issue asks for, in a stable order.
func TestDoctorChecksTheFourThings(t *testing.T) {
	t.Parallel()

	checks := doctorChecks(healthyDoctorDeps())

	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	assert.Equal(t, []string{"database", "migrations", "rpc", "passphrase"}, names)
}

// TestDoctorAllChecksPass proves a healthy deployment is reported as healthy
// and exits zero.
func TestDoctorAllChecksPass(t *testing.T) {
	t.Parallel()

	out, err := runDoctorCmd(t, healthyDoctorDeps())
	require.NoError(t, err)

	assert.Contains(t, out, "PASS  database")
	assert.Contains(t, out, "PASS  migrations")
	assert.Contains(t, out, "PASS  rpc")
	assert.Contains(t, out, "PASS  passphrase")
	assert.NotContains(t, out, "FAIL")
}

// TestDoctorReportsFailures proves a failing check is reported as a failure
// and that the command ends with a non-zero status.
func TestDoctorReportsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// broken breaks one thing.
		broken func(*doctorDeps)
		// wantErr is what the report says about it.
		wantErr string
		// wantFailed is how many checks fail. An unreachable endpoint
		// fails two: it cannot be reached, and its passphrase cannot be
		// checked either.
		wantFailed int
	}{
		{
			name: "the database is unreachable",
			broken: func(d *doctorDeps) {
				d.Ping = func(context.Context) error { return errors.New("connection refused") }
			},
			wantErr:    "connection refused",
			wantFailed: 1,
		},
		{
			name: "the schema is behind",
			broken: func(d *doctorDeps) {
				d.Migrations = func(context.Context) (uint, uint, bool, error) { return 1, 2, false, nil }
			},
			wantErr:    "version 2 is available",
			wantFailed: 1,
		},
		{
			name: "the schema is dirty",
			broken: func(d *doctorDeps) {
				d.Migrations = func(context.Context) (uint, uint, bool, error) { return 2, 2, true, nil }
			},
			wantErr:    "marked dirty",
			wantFailed: 1,
		},
		{
			name: "the endpoint is unreachable",
			broken: func(d *doctorDeps) {
				d.Passphrase = func(context.Context) (string, error) { return "", errors.New("rpc is down") }
			},
			wantErr:    "rpc is down",
			wantFailed: 2,
		},
		{
			name: "the endpoint serves another network",
			broken: func(d *doctorDeps) {
				d.Passphrase = func(context.Context) (string, error) { return stellar.NetworkPublic, nil }
			},
			wantErr:    "serves a different network",
			wantFailed: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deps := healthyDoctorDeps()
			tt.broken(&deps)

			out, err := runDoctorCmd(t, deps)
			require.Error(t, err, "a failed check must end with a non-zero status")
			assert.Contains(t, err.Error(), fmt.Sprintf("%d of 4 checks failed", tt.wantFailed))
			assert.Contains(t, out, "FAIL")
			assert.Contains(t, out, tt.wantErr)
		})
	}
}

// TestDoctorReportsEveryFailure proves one broken thing does not hide the
// others: the point of a doctor run is to say everything that is wrong.
func TestDoctorReportsEveryFailure(t *testing.T) {
	t.Parallel()

	deps := healthyDoctorDeps()
	deps.Ping = func(context.Context) error { return errors.New("connection refused") }
	deps.Passphrase = func(context.Context) (string, error) { return "", errors.New("rpc is down") }

	out, err := runDoctorCmd(t, deps)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "3 of 4 checks failed")
	assert.Contains(t, out, "connection refused")
	assert.Contains(t, out, "rpc is down")
}

// TestDoctorJSONOutput proves the report can be read by a machine.
func TestDoctorJSONOutput(t *testing.T) {
	t.Parallel()

	out, err := runDoctorCmd(t, healthyDoctorDeps(), "--json")
	require.NoError(t, err)

	var report doctorReport
	require.NoError(t, json.Unmarshal([]byte(out), &report), "output was not JSON: %s", out)

	assert.True(t, report.OK)
	require.Len(t, report.Checks, 4)
	assert.Equal(t, "database", report.Checks[0].Name)
	assert.True(t, report.Checks[0].OK)
	assert.NotEmpty(t, report.Checks[0].Detail)
}

// TestDoctorJSONOutputOnFailure proves a failing check is in the JSON too.
func TestDoctorJSONOutputOnFailure(t *testing.T) {
	t.Parallel()

	deps := healthyDoctorDeps()
	deps.Ping = func(context.Context) error { return errors.New("connection refused") }

	out, err := runDoctorCmd(t, deps, "--json")
	require.Error(t, err)

	var report doctorReport
	require.NoError(t, json.Unmarshal([]byte(out), &report), "output was not JSON: %s", out)

	assert.False(t, report.OK)
	require.Len(t, report.Checks, 4)

	var failed int
	for _, c := range report.Checks {
		if !c.OK {
			failed++
			assert.Contains(t, c.Error, "connection refused")
			assert.Empty(t, c.Detail)
		}
	}
	assert.Equal(t, 1, failed)
}

// TestDoctorReadsTheEnvironmentConfiguration proves the command's
// dependencies come from the same configuration as everything else.
func TestDoctorReadsTheEnvironmentConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://sorovault:sorovault@localhost:5432/sorovault")
	t.Setenv("RPC_URL", "https://rpc.example.test")
	t.Setenv("NETWORK_PASSPHRASE", stellar.NetworkPublic)

	deps, err := doctorDepsFromEnv()
	require.NoError(t, err)
	t.Cleanup(func() { _ = deps.Close() })

	assert.Equal(t, stellar.NetworkPublic, deps.ExpectPassphrase)
}

// TestDoctorNeedsADatabaseURL proves a deployment without one is reported as
// a configuration error rather than a passing check.
func TestDoctorNeedsADatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", "")

	_, err := doctorDepsFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

// TestDoctorDoesNotModifyTheDatabase proves the doctor only reads: it runs
// against a real database and leaves the schema exactly as it found it.
//
// The schema used here has no migrations applied, so the check that reads
// the migration version must not leave a version table behind either.
func TestDoctorDoesNotModifyTheDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	schema := "doctor_readonly"
	createSchema(t, url, schema)
	t.Cleanup(func() { dropSchema(t, url, schema) })

	// A connection string that looks at the empty schema and nowhere else.
	scoped := url + "&options=-c%20search_path%3D" + schema

	before := tableCounts(t, scoped)
	assert.Empty(t, before, "the schema starts empty")

	cfg := &config.Config{
		DatabaseURL:       scoped,
		RPCURL:            "http://127.0.0.1:1", // unreachable on purpose
		NetworkPassphrase: stellar.NetworkTestnet,
	}
	deps, err := newDoctorDeps(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = deps.Close() })

	report := runDoctor(ctx, doctorChecks(deps))

	// The schema has no migrations applied, so the check says so — and it
	// says so without having created the table it would have read.
	var migrations doctorResult
	for _, c := range report.Checks {
		if c.Name == "migrations" {
			migrations = c
		}
	}
	assert.False(t, migrations.OK, "an empty schema is behind the latest migration")
	assert.Contains(t, migrations.Error, "version 2 is available")

	after := tableCounts(t, scoped)
	assert.Equal(t, before, after, "the doctor must not change the database")
	assert.Empty(t, after, "the doctor must not create a migration table")
}

// TestDoctorClosesItsDependencies proves the command lets go of what it
// opened.
func TestDoctorClosesItsDependencies(t *testing.T) {
	t.Parallel()

	closed := false
	deps := healthyDoctorDeps()
	deps.Close = func() error {
		closed = true
		return nil
	}

	_, err := runDoctorCmd(t, deps)
	require.NoError(t, err)
	assert.True(t, closed, "the doctor must close what it opened")
}

// TestDoctorReportCountsFailures proves the summary is right.
func TestDoctorReportCountsFailures(t *testing.T) {
	t.Parallel()

	deps := healthyDoctorDeps()
	deps.Ping = func(context.Context) error { return errors.New("connection refused") }
	deps.Migrations = func(context.Context) (uint, uint, bool, error) { return 1, 2, false, nil }

	report := runDoctor(context.Background(), doctorChecks(deps))
	assert.False(t, report.OK)
	assert.Equal(t, 2, report.failed())
	assert.Len(t, report.Checks, 4)
}

// TestDoctorRejectsArguments proves the command takes none.
func TestDoctorRejectsArguments(t *testing.T) {
	t.Parallel()

	_, err := runDoctorCmd(t, healthyDoctorDeps(), "extra")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
}

// createSchema creates an empty schema in the test database.
func createSchema(t *testing.T, url, schema string) {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(), `CREATE SCHEMA IF NOT EXISTS "`+schema+`"`)
	require.NoError(t, err)
}

// dropSchema removes a schema and everything in it.
func dropSchema(t *testing.T, url, schema string) {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	require.NoError(t, err)
}

// tableCounts reports how many rows each table in the connection's schema
// holds, so a test can prove that nothing changed.
func tableCounts(t *testing.T, url string) map[string]int64 {
	t.Helper()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() ORDER BY table_name`)
	require.NoError(t, err)
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())

	counts := map[string]int64{}
	for _, table := range tables {
		var n int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM "`+table+`"`).Scan(&n))
		counts[table] = n
	}
	return counts
}
