package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/store"
)

// TestDatabaseURLEnv names the variable that points the suite at a real
// Postgres. When it is unset the Postgres cases skip, so `go test ./...`
// needs no database and no network.
const TestDatabaseURLEnv = "TEST_DATABASE_URL"

// factory builds a fresh, empty store for one subtest.
type factory func(t *testing.T) store.Store

// TestStore runs one conformance suite against every implementation, so the
// in-memory store used by other packages' tests cannot drift from the
// Postgres one it stands in for.
func TestStore(t *testing.T) {
	t.Parallel()

	impls := map[string]factory{
		"memory": func(t *testing.T) store.Store { return store.NewMemory() },
	}
	if url := os.Getenv(TestDatabaseURLEnv); url != "" {
		impls["postgres"] = postgresFactory(t, url)
	}

	for name, newStore := range impls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runStoreSuite(t, newStore)
		})
	}
}

// TestPostgresIsExercised fails if a CI run silently skipped the Postgres
// half of the suite, which would let the two implementations diverge unseen.
func TestPostgresIsExercised(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("only enforced in CI")
	}
	require.NotEmpty(t, os.Getenv(TestDatabaseURLEnv),
		TestDatabaseURLEnv+" must be set in CI so the Postgres store is covered")
}

// postgresFactory prepares a shared pool and hands each subtest a store over
// a schema of its own, so cases stay isolated while sharing one connection.
func postgresFactory(t *testing.T, url string) factory {
	t.Helper()

	require.NoError(t, store.Migrate(url), "migrating test database")

	pool, err := pgxpool.New(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(context.Background()))

	return func(t *testing.T) store.Store {
		// Truncating rather than recreating keeps setup cheap; the cascade
		// clears specs along with contracts.
		_, err := pool.Exec(context.Background(), "TRUNCATE contracts CASCADE")
		require.NoError(t, err)

		return store.NewPostgresFromPool(pool)
	}
}

// iface builds a small interface with a single named function.
func iface(fnName string) *model.Interface {
	return &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions: []model.Function{{
			Name:    fnName,
			Inputs:  []model.Param{{Name: "who", Type: model.Scalar(model.KindAddress)}},
			Outputs: []model.Type{model.Scalar(model.KindU32)},
		}},
		Types:  model.Types{Structs: []model.Struct{}, Unions: []model.Union{}, Enums: []model.Enum{}, ErrorEnums: []model.ErrorEnum{}},
		Events: []model.Event{},
	}
}

func contract(id, network, hash string) store.Contract {
	return store.Contract{ContractID: id, Network: network, CurrentWasmHash: hash}
}

const (
	idA = "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"
	idB = "CDZZZPYQALFRBQAZD5KCW2SZK4ZF455PJVD7H53AGHXTWAJO4BYVPP2V"
)

func runStoreSuite(t *testing.T, newStore factory) {
	t.Run("catalogued wasm does not require a contract", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.SaveWasmSpec(ctx, "testnet", "hash1", iface("proposed")))
		got, err := s.GetWasmSpec(ctx, "testnet", "hash1")
		require.NoError(t, err)
		assert.Equal(t, "proposed", got.Interface.Functions[0].Name)
		_, err = s.GetContract(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound, "cataloguing code must not create a contract row")
	})

	t.Run("save and read back", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))

		got, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Equal(t, idA, got.ContractID)
		assert.Equal(t, "hash1", got.CurrentWasmHash)
		assert.False(t, got.FirstSeen.IsZero())

		spec, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "hash1", spec.WasmHash)
		require.Len(t, spec.Interface.Functions, 1)
		assert.Equal(t, "balance", spec.Interface.Functions[0].Name)
		assert.Equal(t, "Address", spec.Interface.Functions[0].Inputs[0].Type.Display)

		require.NoError(t, s.UpdateSource(ctx, "testnet", idA, "https://github.com/example/repo", "0123456789abcdef0123456789abcdef01234567"))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Equal(t, "https://github.com/example/repo", got.SourceURL)
		assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", got.SourceCommit)

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))
		got, err = s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Equal(t, "https://github.com/example/repo", got.SourceURL, "refresh must preserve source metadata")
	})

	t.Run("missing rows report ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		_, err := s.GetContract(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound)

		_, err = s.GetSpec(ctx, "testnet", idA, "")
		assert.ErrorIs(t, err, store.ErrNotFound)

		_, err = s.ListVersions(ctx, "testnet", idA)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("resaving the same hash does not add a version", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("balance")))

		versions, err := s.ListVersions(ctx, "testnet", idA)
		require.NoError(t, err)
		assert.Len(t, versions, 1, "re-registering an unchanged contract must be idempotent")
	})

	t.Run("upgrading keeps the prior version", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("old_fn")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash2"), iface("new_fn")))

		current, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "hash2", current.WasmHash)
		assert.Equal(t, "new_fn", current.Interface.Functions[0].Name)

		// The whole point of versioning: the superseded interface is still
		// retrievable by its hash.
		old, err := s.GetSpec(ctx, "testnet", idA, "hash1")
		require.NoError(t, err)
		assert.Equal(t, "old_fn", old.Interface.Functions[0].Name)

		versions, err := s.ListVersions(ctx, "testnet", idA)
		require.NoError(t, err)
		require.Len(t, versions, 2)

		var currentCount int
		for _, v := range versions {
			if v.Current {
				currentCount++
				assert.Equal(t, "hash2", v.WasmHash)
			}
		}
		assert.Equal(t, 1, currentCount, "exactly one version is current")
	})

	t.Run("first_seen survives a refresh", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("f")))
		first, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)

		time.Sleep(5 * time.Millisecond)
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash2"), iface("f")))

		after, err := s.GetContract(ctx, "testnet", idA)
		require.NoError(t, err)

		assert.WithinDuration(t, first.FirstSeen, after.FirstSeen, time.Millisecond,
			"first_seen records when the contract was first registered, not last refreshed")
		assert.False(t, after.LastRefreshed.Before(first.LastRefreshed))
	})

	t.Run("unknown version hash is not found", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash1"), iface("f")))

		_, err := s.GetSpec(ctx, "testnet", idA, "nonexistent")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("networks are separate namespaces", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "hash-t"), iface("on_testnet")))
		require.NoError(t, s.Save(ctx, contract(idA, "public", "hash-p"), iface("on_public")))

		testnet, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "on_testnet", testnet.Interface.Functions[0].Name)

		public, err := s.GetSpec(ctx, "public", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "on_public", public.Interface.Functions[0].Name)

		page, err := s.ListContracts(ctx, store.ListFilter{Network: "public"})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)
	})

	t.Run("network listing is distinct and sorted", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idB, "public", "h2"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h3"), iface("f")))

		networks, err := s.ListNetworks(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"public", "testnet"}, networks)
	})

	t.Run("listing filters and pages", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("f")))

		all, err := s.ListContracts(ctx, store.ListFilter{})
		require.NoError(t, err)
		assert.Equal(t, 2, all.Total)
		assert.Len(t, all.Contracts, 2)

		// Both fixture IDs share a "CDZZ" prefix, so the query has to be
		// long enough to discriminate.
		match, err := s.ListContracts(ctx, store.ListFilter{Query: "CDZZXV"})
		require.NoError(t, err)
		require.Equal(t, 1, match.Total)
		assert.Equal(t, idA, match.Contracts[0].ContractID)

		none, err := s.ListContracts(ctx, store.ListFilter{Query: "no-such-contract"})
		require.NoError(t, err)
		assert.Zero(t, none.Total)
		assert.Empty(t, none.Contracts, "an empty result must be [] rather than nil")

		firstPage, err := s.ListContracts(ctx, store.ListFilter{Limit: 1})
		require.NoError(t, err)
		assert.Len(t, firstPage.Contracts, 1)
		assert.Equal(t, 2, firstPage.Total, "total counts matches, not the page")

		secondPage, err := s.ListContracts(ctx, store.ListFilter{Limit: 1, Offset: 1})
		require.NoError(t, err)
		require.Len(t, secondPage.Contracts, 1)
		assert.NotEqual(t, firstPage.Contracts[0].ContractID, secondPage.Contracts[0].ContractID)

		past, err := s.ListContracts(ctx, store.ListFilter{Limit: 10, Offset: 99})
		require.NoError(t, err)
		assert.Empty(t, past.Contracts)
		assert.Equal(t, 2, past.Total, "paging past the end still reports the true total")
	})

	t.Run("search is case-insensitive", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "cdzzxv"})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)
	})

	t.Run("search matches interface symbols", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		rich := iface("get_proposal")
		rich.Functions = append(rich.Functions, model.Function{Name: "vote"})
		rich.Types.Structs = []model.Struct{{Name: "Proposal", Fields: []model.StructField{}}}
		rich.Events = []model.Event{{Name: "Voted", Params: []model.EventParam{}}}
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), rich))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("transfer")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "PROPOSAL"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, idA, page.Contracts[0].ContractID)
		assert.Equal(t, []string{"Proposal", "get_proposal"}, page.Contracts[0].Matches)

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "vote"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, []string{"Voted", "vote"}, page.Contracts[0].Matches,
			"events are searchable too")
	})

	t.Run("search uses only the current interface", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("legacy_mint")))
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h2"), iface("mint")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "legacy"})
		require.NoError(t, err)
		assert.Zero(t, page.Total, "a function removed by an upgrade must stop matching")

		page, err = s.ListContracts(ctx, store.ListFilter{Query: "mint"})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Equal(t, []string{"mint"}, page.Contracts[0].Matches)
	})

	t.Run("an id match carries no symbol matches", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: idA[:10]})
		require.NoError(t, err)
		require.Equal(t, 1, page.Total)
		assert.Empty(t, page.Contracts[0].Matches)

		page, err = s.ListContracts(ctx, store.ListFilter{})
		require.NoError(t, err)
		assert.Empty(t, page.Contracts[0].Matches, "an unfiltered listing reports no matches")
	})

	t.Run("symbol search total is stable past the last page", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("swap")))
		require.NoError(t, s.Save(ctx, contract(idB, "testnet", "h2"), iface("swap_exact")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "swap", Offset: 10})
		require.NoError(t, err)
		assert.Empty(t, page.Contracts)
		assert.Equal(t, 2, page.Total)
	})

	t.Run("wildcards in a query are literal", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		// "%" must match a literal percent sign, not every row.
		for _, q := range []string{"%", "_", "%%"} {
			page, err := s.ListContracts(ctx, store.ListFilter{Query: q})
			require.NoError(t, err)
			assert.Zero(t, page.Total, "query %q must be treated as literal text", q)
		}
	})

	t.Run("query is trimmed", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Query: "  CDZZXV  "})
		require.NoError(t, err)
		assert.Equal(t, 1, page.Total)
	})

	t.Run("limit is clamped", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), iface("f")))

		page, err := s.ListContracts(ctx, store.ListFilter{Limit: 10_000})
		require.NoError(t, err)
		assert.Len(t, page.Contracts, 1, "an oversized limit must be clamped, not rejected")
	})

	t.Run("saving a nil interface is rejected", func(t *testing.T) {
		s := newStore(t)

		err := s.Save(context.Background(), contract(idA, "testnet", "h1"), nil)
		require.Error(t, err)
	})

	t.Run("stored specs are independent of the caller's value", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		original := iface("original")
		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), original))

		// Mutating what was passed in, or what was handed back, must not
		// disturb what the store holds.
		original.Functions[0].Name = "mutated"

		got, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		require.Equal(t, "original", got.Interface.Functions[0].Name)

		got.Interface.Functions[0].Name = "mutated-again"

		again, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, "original", again.Interface.Functions[0].Name)
	})

	t.Run("round trips a rich interface", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		elem := model.Scalar(model.KindI128)
		rich := iface("complex")
		rich.Meta = map[string]string{"rsver": "1.93.0"}
		rich.Functions[0].Outputs = []model.Type{{
			Kind: model.KindVec, Element: &elem, Display: "Vec<i128>",
		}}
		rich.Types.Structs = []model.Struct{{
			Name:    "Pair",
			IsTuple: true,
			Fields: []model.StructField{
				{Name: "0", Type: model.Scalar(model.KindU32)},
				{Name: "1", Type: model.Scalar(model.KindU32)},
			},
		}}
		rich.Events = []model.Event{{
			Name:       "Transfer",
			DataFormat: "map",
			Params:     []model.EventParam{{Name: "from", Type: model.Scalar(model.KindAddress), Location: "topic-list"}},
		}}

		require.NoError(t, s.Save(ctx, contract(idA, "testnet", "h1"), rich))

		got, err := s.GetSpec(ctx, "testnet", idA, "")
		require.NoError(t, err)
		assert.Equal(t, rich, got.Interface, "the stored ABI must survive a round trip unchanged")
	})

	t.Run("concurrent saves are safe", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()

		const workers = 8
		errs := make(chan error, workers)
		for i := range workers {
			go func() {
				id := fmt.Sprintf("%s%d", idA[:len(idA)-1], i)
				errs <- s.Save(ctx, contract(id, "testnet", "h1"), iface("f"))
			}()
		}
		for range workers {
			require.NoError(t, <-errs)
		}

		page, err := s.ListContracts(ctx, store.ListFilter{})
		require.NoError(t, err)
		assert.Equal(t, workers, page.Total)
	})
}

func TestListFilterNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		in                    store.ListFilter
		wantLimit, wantOffset int
		wantQuery             string
	}{
		{"zero applies defaults", store.ListFilter{}, store.DefaultLimit, 0, ""},
		{"negative limit", store.ListFilter{Limit: -5}, store.DefaultLimit, 0, ""},
		{"oversized limit is clamped", store.ListFilter{Limit: 5000}, store.MaxLimit, 0, ""},
		{"negative offset", store.ListFilter{Offset: -3}, store.DefaultLimit, 0, ""},
		{"query is trimmed", store.ListFilter{Query: "  abc \n"}, store.DefaultLimit, 0, "abc"},
		{"valid values pass through", store.ListFilter{Limit: 10, Offset: 20}, 10, 20, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := tt.in
			f.Normalize()
			assert.Equal(t, tt.wantLimit, f.Limit)
			assert.Equal(t, tt.wantOffset, f.Offset)
			assert.Equal(t, tt.wantQuery, f.Query)
		})
	}
}
