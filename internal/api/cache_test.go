package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
)

func TestInterfaceCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newInterfaceCache(16)
	a := specCacheKey{network: "n", contractID: "c", wasmHash: "a"}
	b := specCacheKey{network: "n", contractID: "c", wasmHash: "b"}
	c := specCacheKey{network: "n", contractID: "c", wasmHash: "c"}
	value := cachedInterface{body: []byte("12345"), decodedAt: time.Unix(1, 0)}

	cache.add(a, value)
	cache.add(b, value)
	if _, ok := cache.get(a); !ok {
		t.Fatal("expected cache hit for recently used entry")
	}
	cache.add(c, value)

	if _, ok := cache.get(b); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	if _, ok := cache.get(a); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := cache.get(c); !ok {
		t.Fatal("new entry was not retained")
	}
}

func TestInterfaceCacheDoesNotStoreOversizedValue(t *testing.T) {
	cache := newInterfaceCache(3)
	key := specCacheKey{network: "n", contractID: "c", wasmHash: "h"}
	cache.add(key, cachedInterface{body: []byte("interface")})

	if _, ok := cache.get(key); ok {
		t.Fatal("value larger than the cache budget was retained")
	}
}

func BenchmarkGetContractCached(b *testing.B) {
	b.Run("cached", func(b *testing.B) { benchmarkGetContract(b, true) })
	b.Run("uncached", func(b *testing.B) { benchmarkGetContract(b, false) })
}

func benchmarkGetContract(b *testing.B, cached bool) {
	const contractID = "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := store.NewMemory()
	iface := &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions:  []model.Function{},
		Types: model.Types{
			Structs: []model.Struct{}, Unions: []model.Union{},
			Enums: []model.Enum{}, ErrorEnums: []model.ErrorEnum{},
		},
		Events: []model.Event{},
	}
	if err := db.Save(context.Background(), store.Contract{
		ContractID: contractID, Network: "testnet", CurrentWasmHash: "benchmark-hash",
	}, iface); err != nil {
		b.Fatal(err)
	}
	reg := registry.New(stellartest.New(), spec.XDRDecoder{}, db, registry.Options{Logger: log})
	server := NewServer(reg, log)
	if !cached {
		server.cache.capacity = 0
	}
	r := chi.NewRouter()
	r.Mount("/api", server.Routes())
	request := httptest.NewRequest(http.MethodGet, "/api/contracts/"+contractID, nil)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		b.Fatalf("warming cache: got status %d", response.Code)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		response = httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("request: got status %d", response.Code)
		}
	}
}
