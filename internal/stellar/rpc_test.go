package stellar_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/wasm"
)

// A real testnet contract address, used so the strkey decoding under test
// operates on a genuine value rather than a hand-rolled one.
const testContractID = "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"

// rpcRequest is the subset of a JSON-RPC request the fake server inspects.
type rpcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
}

// fakeRPC is a JSON-RPC server standing in for Stellar RPC.
type fakeRPC struct {
	t *testing.T

	// entries maps a base64 ledger key to the base64 LedgerEntryData to
	// return for it. A key that is absent comes back as no entry, which is
	// how the network reports a missing or expired entry.
	entries map[string]string

	passphrase string

	// failMethod, when set, makes that method return a JSON-RPC error.
	failMethod string

	// calls records the methods invoked, so tests can assert how many round
	// trips an operation cost.
	calls []string
}

func newFakeRPC(t *testing.T) *fakeRPC {
	return &fakeRPC{
		t:          t,
		entries:    map[string]string{},
		passphrase: stellar.NetworkTestnet,
	}
}

func (f *fakeRPC) start() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *fakeRPC) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decoding rpc request: %v", err)
		return
	}
	f.calls = append(f.calls, req.Method)

	w.Header().Set("Content-Type", "application/json")

	if req.Method == f.failMethod {
		f.writeError(w, req.ID, "simulated backend failure")
		return
	}

	switch req.Method {
	case "getNetwork":
		f.writeResult(w, req.ID, map[string]any{
			"passphrase":      f.passphrase,
			"protocolVersion": 23,
		})

	case "getLedgerEntries":
		var params struct {
			Keys []string `json:"keys"`
		}
		require.NoError(f.t, json.Unmarshal(req.Params, &params))

		results := []map[string]any{}
		for _, k := range params.Keys {
			if data, ok := f.entries[k]; ok {
				results = append(results, map[string]any{
					"key":                   k,
					"xdr":                   data,
					"lastModifiedLedgerSeq": 100,
				})
			}
		}
		f.writeResult(w, req.ID, map[string]any{"entries": results, "latestLedger": 101})

	default:
		f.writeError(w, req.ID, "unexpected method "+req.Method)
	}
}

func (f *fakeRPC) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	require.NoError(f.t, json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	}))
}

func (f *fakeRPC) writeError(w http.ResponseWriter, id json.RawMessage, msg string) {
	require.NoError(f.t, json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": msg},
	}))
}

// putInstance registers the contract's instance entry, pointing at wasmHash.
func (f *fakeRPC) putInstance(contractID string, wasmHash xdr.Hash) {
	f.t.Helper()

	key := instanceKey(f.t, contractID)
	instance := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{
			Type:     xdr.ContractExecutableTypeContractExecutableWasm,
			WasmHash: &wasmHash,
		},
	}

	data := xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   contractAddress(f.t, contractID),
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &instance},
		},
	}
	f.entries[key] = marshalBase64(f.t, data)
}

// putAssetContract registers an instance entry for a Stellar asset contract,
// which has no uploaded WASM.
func (f *fakeRPC) putAssetContract(contractID string) {
	f.t.Helper()

	instance := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{
			Type: xdr.ContractExecutableTypeContractExecutableStellarAsset,
		},
	}
	data := xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   contractAddress(f.t, contractID),
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &instance},
		},
	}
	f.entries[instanceKey(f.t, contractID)] = marshalBase64(f.t, data)
}

// putCode registers the contract code entry holding the module bytes.
func (f *fakeRPC) putCode(wasmHash xdr.Hash, code []byte) {
	f.t.Helper()

	var key xdr.LedgerKey
	require.NoError(f.t, key.SetContractCode(wasmHash))
	encoded, err := key.MarshalBinaryBase64()
	require.NoError(f.t, err)

	data := xdr.LedgerEntryData{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.ContractCodeEntry{Hash: wasmHash, Code: code},
	}
	f.entries[encoded] = marshalBase64(f.t, data)
}

func contractAddress(t *testing.T, contractID string) xdr.ScAddress {
	t.Helper()

	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	require.NoError(t, err)
	require.Len(t, raw, 32)

	id := xdr.ContractId(*(*[32]byte)(raw))
	return xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &id,
	}
}

func instanceKey(t *testing.T, contractID string) string {
	t.Helper()

	var key xdr.LedgerKey
	require.NoError(t, key.SetContractData(
		contractAddress(t, contractID),
		xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
		xdr.ContractDataDurabilityPersistent,
	))
	encoded, err := key.MarshalBinaryBase64()
	require.NoError(t, err)
	return encoded
}

func marshalBase64(t *testing.T, v interface{ MarshalBinary() ([]byte, error) }) string {
	t.Helper()

	raw, err := v.MarshalBinary()
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

func hashOf(b byte) xdr.Hash {
	var h xdr.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func TestFetchContract(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()

	wasmHash := hashOf(0xAB)
	code := []byte("\x00asm\x01\x00\x00\x00 pretend module")
	fake.putInstance(testContractID, wasmHash)
	fake.putCode(wasmHash, code)

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	got, err := f.FetchContract(context.Background(), testContractID)
	require.NoError(t, err)

	assert.Equal(t, testContractID, got.ID)
	assert.Equal(t, hex.EncodeToString(wasmHash[:]), got.WasmHash)
	assert.Equal(t, code, got.Wasm)

	assert.Equal(t, []string{"getLedgerEntries", "getLedgerEntries"}, fake.calls,
		"fetching a module takes exactly two round trips: instance, then code")
}

// TestFetchRejectsOversizedCode proves the module bound is applied where the
// bytes arrive, before a hostile RPC response reaches the decoder.
func TestFetchRejectsOversizedCode(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()

	wasmHash := hashOf(0x44)
	fake.putInstance(testContractID, wasmHash)
	fake.putCode(wasmHash, make([]byte, wasm.MaxModuleSize+1))

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	_, err := f.FetchContract(context.Background(), testContractID)
	require.ErrorIs(t, err, wasm.ErrModuleTooLarge)

	_, err = f.FetchWasm(context.Background(), hex.EncodeToString(wasmHash[:]))
	require.ErrorIs(t, err, wasm.ErrModuleTooLarge)
}

// TestWasmHashIsOneRoundTrip is the property that makes refresh cheap: an
// unchanged contract must not download its module.
func TestWasmHashIsOneRoundTrip(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()

	wasmHash := hashOf(0x11)
	fake.putInstance(testContractID, wasmHash)
	// Deliberately no code entry: reaching for one would be a bug.

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	got, err := f.WasmHash(context.Background(), testContractID)
	require.NoError(t, err)

	assert.Equal(t, hex.EncodeToString(wasmHash[:]), got)
	assert.Len(t, fake.calls, 1, "reading the hash must not fetch the module")
}

func TestFetchContractNotFound(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	_, err := f.FetchContract(context.Background(), testContractID)
	require.ErrorIs(t, err, stellar.ErrContractNotFound)
	assert.Contains(t, err.Error(), testContractID)
}

func TestFetchContractNoWasm(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()
	fake.putAssetContract(testContractID)

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	_, err := f.FetchContract(context.Background(), testContractID)
	require.ErrorIs(t, err, stellar.ErrNoWasm,
		"a Stellar asset contract has no module and must be distinguishable from a missing one")
}

// TestFetchContractMissingCodeEntry covers an instance that points at a WASM
// whose code entry has expired without being restored.
func TestFetchContractMissingCodeEntry(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()
	fake.putInstance(testContractID, hashOf(0x22))

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	_, err := f.FetchContract(context.Background(), testContractID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no code entry")
	assert.NotErrorIs(t, err, stellar.ErrContractNotFound,
		"the contract exists; it is its code entry that is gone")
}

func TestFetchContractRPCError(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	fake.failMethod = "getLedgerEntries"
	srv := fake.start()

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	_, err := f.FetchContract(context.Background(), testContractID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "getLedgerEntries")
}

func TestFetchContractInvalidID(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	for _, id := range []string{
		"",
		"not-an-address",
		// A well-formed account address, which is the wrong strkey kind.
		"GCHPJMNH7WWIHX7CY5CKWR3I35A5DK4X6IU7CJSFUDHTBEWWMI6VEHFJ",
	} {
		_, err := f.FetchContract(context.Background(), id)
		require.Error(t, err, "id %q must be rejected", id)
		assert.Empty(t, fake.calls, "a malformed address must not cost a round trip")
	}
}

func TestNetwork(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	got, err := f.Network(context.Background())
	require.NoError(t, err)
	assert.Equal(t, stellar.NetworkTestnet, got)
}

func TestNetworkName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{stellar.NetworkPublic, "public"},
		{stellar.NetworkTestnet, "testnet"},
		{stellar.NetworkFuturenet, "futurenet"},
		{"", "unknown"},
		// An unrecognised passphrase is passed through so that a private
		// network still gets its own stable namespace.
		{"Standalone Network ; February 2017", "Standalone Network ; February 2017"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, stellar.NetworkName(tt.in))
	}
}

func TestValidateContractID(t *testing.T) {
	t.Parallel()

	require.NoError(t, stellar.ValidateContractID(testContractID))

	for _, bad := range []string{"", "C", "nope", "GCHPJMNH7WWIHX7CY5CKWR3I35A5DK4X6IU7CJSFUDHTBEWWMI6VEHFJ"} {
		assert.Error(t, stellar.ValidateContractID(bad), "expected %q to be rejected", bad)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()

	fake := newFakeRPC(t)
	srv := fake.start()
	fake.putInstance(testContractID, hashOf(0x33))

	f := stellar.NewRPCFetcher(srv.URL, srv.Client())
	t.Cleanup(func() { _ = f.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.FetchContract(ctx, testContractID)
	require.Error(t, err)
}
