package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/store"
)

const listBenchmarkContracts = 10_000

type listBenchmarkBackend struct {
	name    string
	network string
	store   store.Store
}

type listBenchmarkQuery struct {
	name  string
	query string
	want  int
}

func BenchmarkListContracts(b *testing.B) {
	backends := listBenchmarkBackends(b)
	queries := []listBenchmarkQuery{
		{name: "many", query: "transfer", want: listBenchmarkContracts},
		{name: "few", query: "reconcile_rare", want: listBenchmarkContracts / 100},
		{name: "none", query: "symbol_not_present", want: 0},
	}

	for _, backend := range backends {
		b.Run(backend.name, func(b *testing.B) {
			for _, query := range queries {
				b.Run(query.name, func(b *testing.B) {
					filter := store.ListFilter{Network: backend.network, Query: query.query}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						page, err := backend.store.ListContracts(context.Background(), filter)
						if err != nil {
							b.Fatal(err)
						}
						if page.Total != query.want {
							b.Fatalf("query %q matched %d contracts, want %d", query.query, page.Total, query.want)
						}
					}
				})
			}
		})
	}
}

func listBenchmarkBackends(b *testing.B) []listBenchmarkBackend {
	b.Helper()
	b.StopTimer()

	ctx := context.Background()
	network := fmt.Sprintf("bench-store-search-%d", os.Getpid())
	memory := store.NewMemory()
	if err := seedListBenchmarkStore(ctx, memory, network); err != nil {
		b.Fatal(err)
	}
	backends := []listBenchmarkBackend{{name: "memory", network: network, store: memory}}

	if url := os.Getenv(TestDatabaseURLEnv); url != "" {
		if err := store.Migrate(url); err != nil {
			b.Fatal(err)
		}
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			b.Fatal(err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			b.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM contracts WHERE network = $1", network); err != nil {
			pool.Close()
			b.Fatal(err)
		}
		postgres := store.NewPostgresFromPool(pool)
		if err := seedListBenchmarkStore(ctx, postgres, network); err != nil {
			pool.Close()
			b.Fatal(err)
		}
		b.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "DELETE FROM contracts WHERE network = $1", network)
			pool.Close()
		})
		backends = append(backends, listBenchmarkBackend{name: "postgres", network: network, store: postgres})
	}

	b.StartTimer()
	return backends
}

func seedListBenchmarkStore(ctx context.Context, backend store.Store, network string) error {
	for i := 0; i < listBenchmarkContracts; i++ {
		functions := []model.Function{
			{Name: "transfer"},
			{Name: "get_balance"},
			{Name: "withdraw"},
		}
		if i%100 == 0 {
			functions = append(functions, model.Function{Name: "reconcile_rare"})
		}
		iface := &model.Interface{
			ABIVersion: model.ABIVersion,
			Functions:  functions,
			Types: model.Types{
				Structs:    []model.Struct{{Name: "VaultState"}},
				Unions:     []model.Union{},
				Enums:      []model.Enum{{Name: "AssetKind"}},
				ErrorEnums: []model.ErrorEnum{},
			},
			Events: []model.Event{{Name: "TransferEvent"}},
		}
		contract := store.Contract{
			ContractID:      fmt.Sprintf("bench-contract-%05d", i),
			Network:         network,
			CurrentWasmHash: fmt.Sprintf("bench-wasm-%05d", i),
			Name:            fmt.Sprintf("Vault %05d", i),
		}
		if err := backend.Save(ctx, contract, iface); err != nil {
			return fmt.Errorf("seeding contract %d: %w", i, err)
		}
	}
	return nil
}
