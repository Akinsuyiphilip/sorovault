package store_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/store"
)

type listQueryContract struct {
	ContractID string
	Matches    []string
}

type listQuerySnapshot struct {
	Total     int
	Contracts []listQueryContract
}

func snapshotListQuery(page *store.ContractPage) listQuerySnapshot {
	contracts := make([]listQueryContract, len(page.Contracts))
	for i, contract := range page.Contracts {
		contracts[i] = listQueryContract{ContractID: contract.ContractID, Matches: contract.Matches}
	}
	sort.Slice(contracts, func(i, j int) bool {
		return contracts[i].ContractID < contracts[j].ContractID
	})
	return listQuerySnapshot{Total: page.Total, Contracts: contracts}
}

func FuzzListQuery(f *testing.F) {
	for _, query := range []string{
		"", "%", "_", `\`, `%_\`, "percent%", "under_score", `back\slash`,
		"\x00", string([]byte{0xff, '%', '_', '\\'}),
	} {
		f.Add(query)
	}

	ctx := context.Background()
	const networkPrefix = "fuzz-list-query"
	network := fmt.Sprintf("%s-%d", networkPrefix, os.Getpid())
	stores := []store.Store{store.NewMemory()}

	if url := os.Getenv(TestDatabaseURLEnv); url != "" {
		require.NoError(f, store.Migrate(url))
		pool, err := pgxpool.New(ctx, url)
		require.NoError(f, err)
		require.NoError(f, pool.Ping(ctx))
		f.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "DELETE FROM contracts WHERE network = $1", network)
			pool.Close()
		})
		stores = append(stores, store.NewPostgresFromPool(pool))
	}

	fixtures := []struct {
		id, hash, name, symbol string
	}{
		{"search-fuzz-percent", "fuzz-percent", "name%percent", "symbol%percent"},
		{"search-fuzz-underscore", "fuzz-underscore", "name_under", "symbol_under"},
		{"search-fuzz-backslash", "fuzz-backslash", `name\backslash`, `symbol\backslash`},
	}
	for i, backend := range stores {
		for _, fixture := range fixtures {
			contract := store.Contract{
				ContractID: fixture.id, Network: network, CurrentWasmHash: fixture.hash, Name: fixture.name,
			}
			require.NoError(f, backend.Save(ctx, contract, iface(fixture.symbol)), "seeding store %d", i)
		}
	}

	f.Fuzz(func(t *testing.T, query string) {
		var want listQuerySnapshot
		for i, backend := range stores {
			page, err := backend.ListContracts(ctx, store.ListFilter{Network: network, Query: query})
			require.NoError(t, err, "listing store %d for query %q", i, query)
			got := snapshotListQuery(page)
			if i == 0 {
				want = got
				continue
			}
			assert.Equal(t, want, got, "stores disagree for query %q", query)
		}
	})
}
