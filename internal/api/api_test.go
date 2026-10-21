package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/api"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
	"github.com/soroworks/sorovault/internal/wasm"
	"github.com/soroworks/sorovault/internal/wasm/wasmtest"
)

const (
	idA = "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"
	idB = "CDZZZPYQALFRBQAZD5KCW2SZK4ZF455PJVD7H53AGHXTWAJO4BYVPP2V"
)

// harness is an API server over a fake network and an in-memory store.
type harness struct {
	server *httptest.Server
	fake   *stellartest.Fake
	store  store.Store
	api    *api.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	return newHarnessWith(t, registry.Options{})
}

func newHarnessWith(t *testing.T, opts registry.Options) *harness {
	t.Helper()

	return newHarnessWithOptions(t, stellartest.New(), store.NewMemory(), opts)
}

func newHarnessWithStore(t *testing.T, fake *stellartest.Fake, db store.Store) *harness {
	t.Helper()

	return newHarnessWithOptions(t, fake, db, registry.Options{})
}

func newHarnessWithOptions(t *testing.T, fake *stellartest.Fake, db store.Store, opts registry.Options) *harness {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if opts.Logger == nil {
		opts.Logger = log
	}

	reg := registry.New(fake, spec.XDRDecoder{}, db, opts)

	server := api.NewServer(reg, log)
	r := chi.NewRouter()
	r.Mount("/api", server.Routes())

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &harness{server: srv, fake: fake, store: db, api: server}
}

type countingStore struct {
	store.Store
	mu        sync.Mutex
	specReads int
}

func (s *countingStore) GetSpec(ctx context.Context, network, contractID, wasmHash string) (*store.Spec, error) {
	s.mu.Lock()
	s.specReads++
	s.mu.Unlock()
	return s.Store.GetSpec(ctx, network, contractID, wasmHash)
}

func (s *countingStore) reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.specReads
}

func (h *harness) do(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, h.server.URL+path, reader)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.server.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

func (h *harness) decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), "response was not JSON: %s", raw)
	return out
}

func moduleWith(t testing.TB, fnName string) []byte {
	t.Helper()

	entry := xdr.ScSpecEntry{
		Kind: xdr.ScSpecEntryKindScSpecEntryFunctionV0,
		FunctionV0: &xdr.ScSpecFunctionV0{
			Name: xdr.ScSymbol(fnName),
			Inputs: []xdr.ScSpecFunctionInputV0{
				{Name: "who", Type: xdr.ScSpecTypeDef{Type: xdr.ScSpecTypeScSpecTypeAddress}},
			},
			Outputs: []xdr.ScSpecTypeDef{{Type: xdr.ScSpecTypeScSpecTypeU32}},
		},
	}
	body, err := entry.MarshalBinary()
	require.NoError(t, err)

	return wasmtest.Module(wasmtest.Section{Name: wasm.SectionContractSpec, Body: body})
}

// moduleWithStruct builds a module whose contract spec declares one
// user-defined type, so the type filter has something to match.
func moduleWithStruct(t testing.TB, typeName string) []byte {
	t.Helper()

	entry := xdr.ScSpecEntry{
		Kind: xdr.ScSpecEntryKindScSpecEntryUdtStructV0,
		UdtStructV0: &xdr.ScSpecUdtStructV0{
			Name: typeName,
			Fields: []xdr.ScSpecUdtStructFieldV0{
				{Name: "amount", Type: xdr.ScSpecTypeDef{Type: xdr.ScSpecTypeScSpecTypeI128}},
			},
		},
	}
	body, err := entry.MarshalBinary()
	require.NoError(t, err)

	return wasmtest.Module(wasmtest.Section{Name: wasm.SectionContractSpec, Body: body})
}

func TestRegisterContract(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))

	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	got := h.decode(t, raw)
	assert.True(t, got["created"].(bool))

	iface := got["interface"].(map[string]any)
	fns := iface["functions"].([]any)
	require.Len(t, fns, 1)
	assert.Equal(t, "balance", fns[0].(map[string]any)["name"])
}

func TestCatalogUploadedWasm(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	module := moduleWith(t, "proposed")
	hash := h.fake.UploadWasm(module)

	resp, raw := h.do(t, http.MethodPost, "/api/wasm/"+hash, "")
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	got := h.decode(t, raw)
	assert.Equal(t, hash, got["wasm_hash"])
	assert.True(t, got["created"].(bool))
	assert.Equal(t, "proposed", got["interface"].(map[string]any)["functions"].([]any)[0].(map[string]any)["name"])

	resp, raw = h.do(t, http.MethodGet, "/api/wasm/"+hash+"?network=testnet", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Equal(t, hash, h.decode(t, raw)["wasm_hash"])
	_, err := h.store.GetContract(context.Background(), "testnet", idA)
	assert.ErrorIs(t, err, store.ErrNotFound, "cataloguing code must not require or create a contract")
}

// TestListContractsByWasm answers the question a vulnerability in a shared
// WASM raises: which other contracts run this code?
func TestListContractsByWasm(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	module := moduleWith(t, "balance")
	hash := stellartest.HashOf(module)

	// Two instances of the same code: the shape a token template or a
	// factory produces.
	h.fake.Deploy(idA, module)
	h.fake.Deploy(idB, module)

	for _, id := range []string{idA, idB} {
		resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+id+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	}

	resp, raw := h.do(t, http.MethodGet, "/api/wasm/"+hash+"/contracts", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	got := h.decode(t, raw)
	assert.Equal(t, hash, got["wasm_hash"])
	assert.Equal(t, "testnet", got["network"])
	assert.Equal(t, float64(2), got["total"])
	assert.False(t, got["ever"].(bool))

	contracts := got["contracts"].([]any)
	require.Len(t, contracts, 2)
	for _, c := range contracts {
		assert.True(t, c.(map[string]any)["current"].(bool),
			"both contracts run this hash")
	}
}

// TestListContractsByWasmEver covers the wider answer: the contracts that
// ran the code in the past and have since been upgraded.
func TestListContractsByWasmEver(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	module := moduleWith(t, "balance")
	hash := stellartest.HashOf(module)

	h.fake.Deploy(idA, module)
	h.fake.Deploy(idB, module)

	for _, id := range []string{idA, idB} {
		resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+id+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	}

	// One of them is upgraded to different code.
	h.fake.Upgrade(idA, moduleWith(t, "other"))
	resp, _ := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, raw := h.do(t, http.MethodGet, "/api/wasm/"+hash+"/contracts", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Equal(t, float64(1), h.decode(t, raw)["total"],
		"only the contract that still runs the hash is listed")

	resp, raw = h.do(t, http.MethodGet, "/api/wasm/"+hash+"/contracts?ever=true", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	got := h.decode(t, raw)
	assert.True(t, got["ever"].(bool))
	assert.Equal(t, float64(2), got["total"], "the upgraded contract ran the hash too")

	contracts := got["contracts"].([]any)
	require.Len(t, contracts, 2)
	for _, c := range contracts {
		entry := c.(map[string]any)
		if entry["contract_id"] == idA {
			assert.False(t, entry["current"].(bool),
				"the upgraded contract no longer runs the hash")
		} else {
			assert.True(t, entry["current"].(bool))
		}
	}
}

func TestListContractsByWasmBadRequest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	resp, raw := h.do(t, http.MethodGet, "/api/wasm/not-a-hash/contracts", "")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	assert.NotEmpty(t, h.decode(t, raw)["error"])
}

func TestUpdateContractSource(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))
	resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	body := `{"source_url":"https://github.com/soroworks/sorovault","source_commit":"0123456789abcdef0123456789abcdef01234567"}`
	resp, raw := h.do(t, http.MethodPut, "/api/contracts/"+idA+"/source", body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	got := h.decode(t, raw)
	assert.Equal(t, "https://github.com/soroworks/sorovault", got["source_url"])
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", got["source_commit"])

	resp, raw = h.do(t, http.MethodPut, "/api/contracts/"+idB+"/source", body)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
}

// TestRegisterExistingContractReturns200 distinguishes creating a record
// from re-verifying one, which is what makes the endpoint safely retryable.
func TestRegisterExistingContractReturns200(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))

	resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, h.decode(t, raw)["created"].(bool))
}

func TestRegisterContractBadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"not json", "{", http.StatusBadRequest},
		{"missing contract_id", `{}`, http.StatusBadRequest},
		{"blank contract_id", `{"contract_id":"   "}`, http.StatusBadRequest},
		{"malformed contract_id", `{"contract_id":"nope"}`, http.StatusBadRequest},
		{"unknown field", `{"contract_id":"` + idA + `","extra":1}`, http.StatusBadRequest},
		{"two objects", `{"contract_id":"` + idA + `"}{"contract_id":"x"}`, http.StatusBadRequest},
		{"unknown contract", `{"contract_id":"` + idA + `"}`, http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			resp, raw := h.do(t, http.MethodPost, "/api/contracts", tt.body)
			assert.Equal(t, tt.want, resp.StatusCode, string(raw))
			assert.NotEmpty(t, h.decode(t, raw)["error"], "an error response must carry a message")
		})
	}
}

// TestBatchRegisterContracts proves a batch reports one result per
// contract, and that one failure does not stop the rest.
func TestBatchRegisterContracts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))
	// idB is never deployed: it fails the way a single request would.

	body := `{"contract_ids":["` + idA + `","` + idB + `","not-a-contract"]}`
	resp, raw := h.do(t, http.MethodPost, "/api/contracts:batch", body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	got := h.decode(t, raw)
	results := got["results"].([]any)
	require.Len(t, results, 3, "one result per contract, whatever happened to it")

	first := results[0].(map[string]any)
	assert.Equal(t, idA, first["contract_id"])
	assert.Equal(t, "created", first["status"])
	assert.True(t, first["created"].(bool))
	assert.Equal(t, idA, first["contract"].(map[string]any)["contract_id"])

	second := results[1].(map[string]any)
	assert.Equal(t, idB, second["contract_id"])
	assert.Equal(t, "error", second["status"])
	assert.Equal(t, float64(http.StatusNotFound), second["code"],
		"a failure carries the status code it would have had")
	assert.NotEmpty(t, second["error"])

	third := results[2].(map[string]any)
	assert.Equal(t, "error", third["status"])
	assert.Equal(t, float64(http.StatusBadRequest), third["code"],
		"a malformed address would have been a bad request of its own")

	assert.Equal(t, float64(1), got["created"])
	assert.Equal(t, float64(0), got["unchanged"])
	assert.Equal(t, float64(0), got["changed"])
	assert.Equal(t, float64(2), got["failed"])
}

// TestBatchRegisterIsIdempotent proves re-registering a known contract is
// reported as unchanged rather than as a new one.
func TestBatchRegisterIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))

	body := `{"contract_ids":["` + idA + `"]}`
	resp, _ := h.do(t, http.MethodPost, "/api/contracts:batch", body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, raw := h.do(t, http.MethodPost, "/api/contracts:batch", body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	got := h.decode(t, raw)
	result := got["results"].([]any)[0].(map[string]any)
	assert.Equal(t, "unchanged", result["status"])
	assert.False(t, result["created"].(bool))
	assert.Equal(t, float64(1), got["unchanged"])
}

// TestBatchRegisterReportsAnUpgrade proves a contract that was already
// registered but has moved on is reported as changed.
func TestBatchRegisterReportsAnUpgrade(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "v1"))

	body := `{"contract_ids":["` + idA + `"]}`
	resp, _ := h.do(t, http.MethodPost, "/api/contracts:batch", body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	h.fake.Upgrade(idA, moduleWith(t, "v2"))
	resp, raw := h.do(t, http.MethodPost, "/api/contracts:batch", body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	got := h.decode(t, raw)
	result := got["results"].([]any)[0].(map[string]any)
	assert.Equal(t, "changed", result["status"])
	assert.True(t, result["changed"].(bool))
	assert.Equal(t, float64(1), got["changed"], "an upgrade is not a failure")
	assert.Equal(t, float64(0), got["failed"])
}

// TestBatchRegisterBadRequests proves the batch is bounded, and that a
// malformed batch is refused outright.
func TestBatchRegisterBadRequests(t *testing.T) {
	t.Parallel()

	tooMany := make([]string, api.MaxBatchContracts+1)
	for i := range tooMany {
		tooMany[i] = idA
	}

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"not json", "{", http.StatusBadRequest},
		{"no contract ids", `{"contract_ids":[]}`, http.StatusBadRequest},
		{"unknown field", `{"contract_ids":["` + idA + `"],"extra":1}`, http.StatusBadRequest},
		{"too many contracts", `{"contract_ids":[` + strings.Join(quoteAll(tooMany), ",") + `]}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			resp, raw := h.do(t, http.MethodPost, "/api/contracts:batch", tt.body)
			assert.Equal(t, tt.want, resp.StatusCode, string(raw))
			assert.NotEmpty(t, h.decode(t, raw)["error"])
		})
	}
}

// quoteAll quotes a list of contract IDs for a JSON body.
func quoteAll(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = `"` + id + `"`
	}
	return out
}

// TestBatchRegisterCountsPerContract proves a batch draws on the registry's
// budget once per contract, and that a busy registry refuses the contract
// it cannot serve rather than the whole batch.
func TestBatchRegisterCountsPerContract(t *testing.T) {
	t.Parallel()

	h := newBlockingHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))

	finish := h.startRegistration(t, idA)
	defer finish()

	body := `{"contract_ids":["` + idA + `"]}`
	resp, raw := h.do(t, http.MethodPost, "/api/contracts:batch", body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	result := h.decode(t, raw)["results"].([]any)[0].(map[string]any)
	assert.Equal(t, "error", result["status"])
	assert.Equal(t, float64(http.StatusServiceUnavailable), result["code"])
}

// TestUndecodableContractIsUnprocessable separates "we could not find it"
// from "we found it but it has no interface".
func TestUndecodableContractIsUnprocessable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, wasmtest.Module(wasmtest.Section{Name: "producers", Body: []byte("rustc")}))

	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(raw))
}

func TestListContracts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "a"))
	h.fake.Deploy(idB, moduleWith(t, "b"))

	for _, id := range []string{idA, idB} {
		resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+id+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	}

	t.Run("all", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		got := h.decode(t, raw)
		assert.Equal(t, float64(2), got["total"])
		assert.Len(t, got["contracts"].([]any), 2)
		assert.Equal(t, float64(store.DefaultLimit), got["limit"])
	})

	t.Run("search", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?q=CDZZXV", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		got := h.decode(t, raw)
		assert.Equal(t, float64(1), got["total"])
	})

	t.Run("no match is an empty list, not an error", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?q=zzzz", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		got := h.decode(t, raw)
		assert.Equal(t, float64(0), got["total"])
		assert.Equal(t, []any{}, got["contracts"])
	})

	t.Run("paging", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?limit=1&offset=1", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		got := h.decode(t, raw)
		assert.Len(t, got["contracts"].([]any), 1)
		assert.Equal(t, float64(2), got["total"])
		assert.Equal(t, float64(1), got["offset"])
	})

	t.Run("garbage paging falls back to defaults", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?limit=abc&offset=xyz", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, float64(store.DefaultLimit), h.decode(t, raw)["limit"])
	})

	t.Run("limit is clamped", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?limit=99999", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, float64(store.MaxLimit), h.decode(t, raw)["limit"])
	})
}

// TestListContractsFiltersByExactName proves the exact-name filters do
// what they say: an exact match, repeatable, and every name has to match.
func TestListContractsFiltersByExactName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "transfer"))
	h.fake.Deploy(idB, moduleWith(t, "transfer_from"))

	for _, id := range []string{idA, idB} {
		resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+id+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	}

	t.Run("an exact function name", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?function=transfer", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		got := h.decode(t, raw)
		require.Equal(t, float64(1), got["total"])
		contracts := got["contracts"].([]any)
		assert.Equal(t, idA, contracts[0].(map[string]any)["contract_id"],
			"transfer_from is not transfer")
	})

	t.Run("every requested function has to match", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?function=transfer&function=transfer_from", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, float64(0), h.decode(t, raw)["total"])
	})

	t.Run("the filter combines with the query", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts?function=transfer&q=CDZZZPY", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, float64(0), h.decode(t, raw)["total"],
			"the query matches one contract and the function filter the other")
	})
}

// TestListContractsFiltersByExactTypeName proves the type filter matches
// user-defined types by their exact name.
func TestListContractsFiltersByExactTypeName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWithStruct(t, "Balance"))

	resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp, raw := h.do(t, http.MethodGet, "/api/contracts?type=Balance", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, float64(1), h.decode(t, raw)["total"])

	resp, raw = h.do(t, http.MethodGet, "/api/contracts?type=Bal", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, float64(0), h.decode(t, raw)["total"],
		"a type name is matched exactly, not as a prefix")
}

// TestListContractsReportsMissingContracts proves the listing says when a
// contract went missing.
func TestListContractsReportsMissingContracts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))
	resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	h.fake.Remove(idA)
	resp, raw := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.True(t, h.decode(t, raw)["missing"].(bool))

	resp, raw = h.do(t, http.MethodGet, "/api/contracts", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	contracts := h.decode(t, raw)["contracts"].([]any)
	require.Len(t, contracts, 1)
	contract := contracts[0].(map[string]any)
	assert.NotEmpty(t, contract["missing_since"],
		"the listing says when the contract went missing")
}

func TestGetContract(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))
	resp, _ := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	got := h.decode(t, raw)
	assert.Equal(t, idA, got["contract_id"])
	assert.Equal(t, "testnet", got["network"])
	assert.True(t, got["current"].(bool))

	iface := got["interface"].(map[string]any)
	assert.Equal(t, "1", iface["abi_version"])

	fn := iface["functions"].([]any)[0].(map[string]any)
	assert.Equal(t, "balance", fn["name"])
	assert.Equal(t, "u32", fn["outputs"].([]any)[0].(map[string]any)["display"])
}

func TestGetContractCachesInterfaceAndTracksUpgrades(t *testing.T) {
	t.Parallel()

	fake := stellartest.New()
	db := &countingStore{Store: store.NewMemory()}
	h := newHarnessWithStore(t, fake, db)
	fake.Deploy(idA, moduleWith(t, "v1"))

	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	for range 2 {
		resp, raw = h.do(t, http.MethodGet, "/api/contracts/"+idA, "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		functions := h.decode(t, raw)["interface"].(map[string]any)["functions"].([]any)
		assert.Equal(t, "v1", functions[0].(map[string]any)["name"])
	}
	assert.Equal(t, 1, db.reads(), "a cache hit must skip loading and decoding the stored spec")

	fake.Upgrade(idA, moduleWith(t, "v2"))
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	resp, raw = h.do(t, http.MethodGet, "/api/contracts/"+idA, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	got := h.decode(t, raw)
	functions := got["interface"].(map[string]any)["functions"].([]any)
	assert.Equal(t, "v2", functions[0].(map[string]any)["name"])
	assert.True(t, got["current"].(bool))
	assert.Equal(t, 2, db.reads(), "a new WASM hash must load its own interface")
}

func TestGetContractNotFound(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA, "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.NotEmpty(t, h.decode(t, raw)["error"])
}

func TestGetFunction(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))
	h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)

	t.Run("found", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA+"/functions/balance", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, "fn balance(who: Address) -> u32", got["signature"])
		assert.Equal(t, "balance", got["function"].(map[string]any)["name"])
	})

	t.Run("missing function", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA+"/functions/nope", "")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Contains(t, h.decode(t, raw)["error"], "nope")
	})

	t.Run("missing contract", func(t *testing.T) {
		resp, _ := h.do(t, http.MethodGet, "/api/contracts/"+idB+"/functions/balance", "")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestListFunctions(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	v1 := moduleWith(t, "balance")
	h.fake.Deploy(idA, v1)
	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	oldHash := stellartest.HashOf(v1)
	h.fake.Upgrade(idA, moduleWith(t, "total"))
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	t.Run("current interface", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA+"/functions", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, idA, got["contract_id"])
		assert.Equal(t, "testnet", got["network"])
		functions := got["functions"].([]any)
		require.Len(t, functions, 1)
		function := functions[0].(map[string]any)
		assert.Equal(t, "total", function["name"])
		assert.Equal(t, "fn total(who: Address) -> u32", function["signature"])
		assert.Len(t, function, 2, "function listings contain names and signatures, not docs")
	})

	t.Run("previous version selected by wasm_hash", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA+"/functions?wasm_hash="+oldHash, "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		functions := h.decode(t, raw)["functions"].([]any)
		require.Len(t, functions, 1)
		assert.Equal(t, "balance", functions[0].(map[string]any)["name"])
	})

	t.Run("no functions returns an empty list", func(t *testing.T) {
		module := wasmtest.Module(wasmtest.Section{Name: wasm.SectionContractSpec})
		h.fake.Deploy(idB, module)
		resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idB+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

		resp, raw = h.do(t, http.MethodGet, "/api/contracts/"+idB+"/functions", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		assert.Equal(t, []any{}, h.decode(t, raw)["functions"])
	})
}

func TestVersionsAndRefresh(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "v1"))
	h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)

	t.Run("single version", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA+"/versions", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		versions := h.decode(t, raw)["versions"].([]any)
		require.Len(t, versions, 1)
		assert.True(t, versions[0].(map[string]any)["current"].(bool))
	})

	t.Run("refresh with no change", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.False(t, h.decode(t, raw)["changed"].(bool))
	})

	t.Run("refresh after upgrade", func(t *testing.T) {
		h.fake.Upgrade(idA, moduleWith(t, "v2"))

		resp, raw := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.True(t, h.decode(t, raw)["changed"].(bool))

		resp, raw = h.do(t, http.MethodGet, "/api/contracts/"+idA+"/versions", "")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Len(t, h.decode(t, raw)["versions"].([]any), 2)
	})

	t.Run("old version is still served by hash", func(t *testing.T) {
		oldHash := stellartest.HashOf(moduleWith(t, "v1"))

		resp, raw := h.do(t, http.MethodGet, "/api/contracts/"+idA+"?wasm_hash="+oldHash, "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.False(t, got["current"].(bool))

		fns := got["interface"].(map[string]any)["functions"].([]any)
		assert.Equal(t, "v1", fns[0].(map[string]any)["name"])
	})

	t.Run("unknown version hash", func(t *testing.T) {
		resp, _ := h.do(t, http.MethodGet, "/api/contracts/"+idA+"?wasm_hash=deadbeef", "")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestRefreshUnregisteredContract(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "v1"))

	resp, _ := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestNetworkParameterSelectsNamespace covers reading a network other than
// the one this instance is pointed at.
func TestNetworkParameterSelectsNamespace(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))
	h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)

	resp, _ := h.do(t, http.MethodGet, "/api/contracts/"+idA+"?network=public", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the contract is registered on testnet, not public")

	resp, _ = h.do(t, http.MethodGet, "/api/contracts/"+idA+"?network=testnet", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestRealFixtureServesUsableABI drives the real testnet contract through
// the whole HTTP path, which is the deliverable other tooling consumes.
func TestRealFixtureServesUsableABI(t *testing.T) {
	t.Parallel()

	module, err := os.ReadFile("../spec/testdata/zkvote.wasm")
	require.NoError(t, err)

	h := newHarness(t)
	h.fake.Deploy(idA, module)

	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	resp, raw = h.do(t, http.MethodGet, "/api/contracts/"+idA, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	iface := h.decode(t, raw)["interface"].(map[string]any)
	assert.Len(t, iface["functions"].([]any), 7)
	assert.Len(t, iface["events"].([]any), 1)
	assert.Equal(t, "25.3.0#dcbea44513feb7734af6b6c4aced2c4a7a2715d0",
		iface["meta"].(map[string]any)["rssdkver"])

	resp, raw = h.do(t, http.MethodGet, "/api/contracts/"+idA+"/functions/vote", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, h.decode(t, raw)["signature"], "pub_signals: Vec<BytesN<32>>")
}

func TestTypeScriptClient(t *testing.T) {
	t.Parallel()

	module, err := os.ReadFile("../spec/testdata/zkvote.wasm")
	require.NoError(t, err)

	h := newHarness(t)
	h.fake.Deploy(idA, module)
	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	resp, raw = h.do(t, http.MethodGet, "/api/contracts/"+idA+"/client.ts", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	assert.Empty(t, resp.Header.Get("Content-Disposition"))

	body := string(raw)
	assert.Contains(t, body, `export const contractId = "`+idA+`";`)
	assert.Contains(t, body, `export const wasmHash = "`+stellartest.HashOf(module)+`";`)
	assert.Contains(t, body, "has_voted(args: { nullifier: Buffer }")

	resp, _ = h.do(t, http.MethodGet, "/api/contracts/"+idA+"/client.ts?download=1", "")
	assert.Equal(t, `attachment; filename="`+idA+`.client.ts"`, resp.Header.Get("Content-Disposition"))

	resp, _ = h.do(t, http.MethodGet, "/api/contracts/"+idB+"/client.ts", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestInternalErrorsAreNotLeaked checks that an unexpected store failure
// produces a generic 500 rather than exposing internals to the caller.
func TestInternalErrorsAreNotLeaked(t *testing.T) {
	t.Parallel()

	fake := stellartest.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := registry.New(fake, spec.XDRDecoder{}, explodingStore{}, registry.Options{Logger: log})

	r := chi.NewRouter()
	r.Mount("/api", api.NewServer(reg, log).Routes())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/api/contracts")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "internal error", h500Message(t, raw))
	assert.NotContains(t, string(raw), "connection string",
		"internal failures must not surface their detail to callers")
}

func h500Message(t *testing.T, raw []byte) string {
	t.Helper()

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out["error"].(string)
}

// explodingStore fails every read, standing in for a database outage.
type explodingStore struct{ store.Store }

func (explodingStore) ListContracts(context.Context, store.ListFilter) (*store.ContractPage, error) {
	return nil, errDatabaseDown
}

var errDatabaseDown = &databaseDownError{}

type databaseDownError struct{}

func (*databaseDownError) Error() string {
	return "dial tcp: connection string postgres://user:hunter2@db/registry refused"
}
