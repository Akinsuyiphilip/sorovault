package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/store"
)

// TestLatestMigrationVersion proves the embedded migrations can be counted
// without a database, which is what the doctor command compares against.
func TestLatestMigrationVersion(t *testing.T) {
	t.Parallel()

	latest, err := store.LatestMigrationVersion()
	require.NoError(t, err)
	assert.Positive(t, latest, "the repository ships migrations")
}

// TestMigrationStatusMatchesTheMigrator proves the read-only version check
// agrees with golang-migrate about where the database is.
func TestMigrationStatusMatchesTheMigrator(t *testing.T) {
	url := os.Getenv(TestDatabaseURLEnv)
	if url == "" {
		t.Skip(TestDatabaseURLEnv + " not set")
	}
	ctx := context.Background()

	require.NoError(t, store.Migrate(url))

	current, latest, dirty, err := store.MigrationStatus(ctx, url)
	require.NoError(t, err)
	assert.False(t, dirty, "a freshly migrated database is not dirty")
	assert.Equal(t, latest, current, "the database is at the latest migration")

	version, dirty, err := store.MigrationVersion(url)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.Equal(t, version, current, "the read-only check must agree with the migrator")
}
