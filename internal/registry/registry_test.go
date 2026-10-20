package registry_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
	"github.com/soroworks/sorovault/internal/wasm"
	"github.com/soroworks/sorovault/internal/wasm/wasmtest"
)

const (
	idA = "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"
	idB = "CDZZZPYQALFRBQAZD5KCW2SZK4ZF455PJVD7H53AGHXTWAJO4BYVPP2V"
)

// moduleWith builds a WASM module whose contract spec declares one function
// of the given name, so tests can tell versions apart by what they decode to.
func moduleWith(t *testing.T, fnName string) []byte {
	t.Helper()

	entry := xdr.ScSpecEntry{
		Kind: xdr.ScSpecEntryKindScSpecEntryFunctionV0,
		FunctionV0: &xdr.ScSpecFunctionV0{
			Name:    xdr.ScSymbol(fnName),
			Inputs:  []xdr.ScSpecFunctionInputV0{},
			Outputs: []xdr.ScSpecTypeDef{},
		},
	}
	body, err := entry.MarshalBinary()
	require.NoError(t, err)

	return wasmtest.Module(wasmtest.Section{Name: wasm.SectionContractSpec, Body: body})
}

// newRegistry wires a registry over a fake network and an in-memory store.
func newRegistry(t *testing.T) (*registry.Registry, *stellartest.Fake, store.Store) {
	t.Helper()

	fake := stellartest.New()
	db := store.NewMemory()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := registry.New(fake, spec.XDRDecoder{}, db, registry.Options{Logger: log})
	return reg, fake, db
}

func TestRegister(t *testing.T) {
	t.Parallel()

	reg, fake, _ := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	result, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	assert.True(t, result.Created)
	assert.True(t, result.Changed)
	assert.Equal(t, "testnet", result.Contract.Network)
	assert.Equal(t, idA, result.Contract.ContractID)
	require.Len(t, result.Interface.Functions, 1)
	assert.Equal(t, "hello", result.Interface.Functions[0].Name)
}

func TestCatalogWasmWithoutDeployedContract(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	module := moduleWith(t, "proposed")
	hash := stellartest.HashOf(module)
	fake.UploadWasm(module)

	result, err := reg.CatalogWasm(context.Background(), hash)
	require.NoError(t, err)
	assert.Equal(t, hash, result.WasmHash)
	assert.Equal(t, "testnet", result.Network)
	require.Len(t, result.Interface.Functions, 1)
	assert.Equal(t, "proposed", result.Interface.Functions[0].Name)

	stored, err := db.GetWasmSpec(context.Background(), "testnet", hash)
	require.NoError(t, err)
	assert.Equal(t, "proposed", stored.Interface.Functions[0].Name)
	_, err = db.GetContract(context.Background(), "testnet", idA)
	assert.ErrorIs(t, err, store.ErrNotFound, "cataloguing code must not fabricate a deployed contract")
}

func TestRegisterIsIdempotent(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	first, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)
	require.True(t, first.Created)

	second, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	assert.False(t, second.Created, "the contract was already registered")
	assert.False(t, second.Changed, "its WASM has not moved")

	versions, err := db.ListVersions(context.Background(), "testnet", idA)
	require.NoError(t, err)
	assert.Len(t, versions, 1)
}

func TestRegisterDetectsUpgrade(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "v1"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	fake.Upgrade(idA, moduleWith(t, "v2"))

	result, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)
	assert.False(t, result.Created)
	assert.True(t, result.Changed)
	assert.Equal(t, "v2", result.Interface.Functions[0].Name)

	versions, err := db.ListVersions(context.Background(), "testnet", idA)
	require.NoError(t, err)
	assert.Len(t, versions, 2, "the superseded interface must be kept")
}

func TestRegisterRejectsBadID(t *testing.T) {
	t.Parallel()

	reg, fake, _ := newRegistry(t)

	_, err := reg.Register(context.Background(), "not-a-contract")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid contract address")
	assert.Zero(t, fake.Calls["Network"], "a malformed address must be rejected before any RPC")
}

func TestRegisterPropagatesFetchFailure(t *testing.T) {
	t.Parallel()

	reg, _, db := newRegistry(t)

	_, err := reg.Register(context.Background(), idA)
	require.ErrorIs(t, err, stellar.ErrContractNotFound)

	_, err = db.GetContract(context.Background(), "testnet", idA)
	assert.ErrorIs(t, err, store.ErrNotFound, "a failed fetch must not leave a partial record")
}

func TestRegisterPropagatesDecodeFailure(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	// A module with no contract spec section: fetchable, but not decodable.
	fake.Deploy(idA, wasmtest.Module(wasmtest.Section{Name: "producers", Body: []byte("rustc")}))

	_, err := reg.Register(context.Background(), idA)
	require.ErrorIs(t, err, spec.ErrNoSpecSection)

	_, err = db.GetContract(context.Background(), "testnet", idA)
	assert.ErrorIs(t, err, store.ErrNotFound, "an undecodable contract must not be recorded")
}

func TestRefreshUnchanged(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	before := fake.Calls["FetchContract"]

	result, err := reg.Refresh(context.Background(), idA)
	require.NoError(t, err)
	assert.False(t, result.Changed)

	assert.Equal(t, before, fake.Calls["FetchContract"],
		"an unchanged contract must not have its module downloaded again")
	assert.Positive(t, fake.Calls["WasmHash"], "refresh checks the hash first")

	versions, err := db.ListVersions(context.Background(), "testnet", idA)
	require.NoError(t, err)
	assert.Len(t, versions, 1)
}

func TestRefreshBumpsLastRefreshed(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	first, err := db.GetContract(context.Background(), "testnet", idA)
	require.NoError(t, err)

	result, err := reg.Refresh(context.Background(), idA)
	require.NoError(t, err)

	assert.False(t, result.Contract.LastRefreshed.Before(first.LastRefreshed),
		"confirming a contract is still live must advance last_refreshed")
}

func TestRefreshDetectsUpgrade(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "v1"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	fake.Upgrade(idA, moduleWith(t, "v2"))

	result, err := reg.Refresh(context.Background(), idA)
	require.NoError(t, err)

	assert.True(t, result.Changed)
	assert.Equal(t, "v2", result.Interface.Functions[0].Name)
	assert.Equal(t, stellartest.HashOf(moduleWith(t, "v2")), result.Contract.CurrentWasmHash)

	// Both interfaces remain retrievable, which is the point of versioning.
	old, err := db.GetSpec(context.Background(), "testnet", idA, stellartest.HashOf(moduleWith(t, "v1")))
	require.NoError(t, err)
	assert.Equal(t, "v1", old.Interface.Functions[0].Name)

	current, err := db.GetSpec(context.Background(), "testnet", idA, "")
	require.NoError(t, err)
	assert.Equal(t, "v2", current.Interface.Functions[0].Name)
}

// TestRefreshMarksAMissingContract proves that a contract which has gone
// from the network is recorded as missing rather than failing the refresh.
func TestRefreshMarksAMissingContract(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	fake.Remove(idA)

	result, err := reg.Refresh(context.Background(), idA)
	require.NoError(t, err, "a contract that is gone is not a failed refresh")
	assert.True(t, result.Missing)
	require.NotNil(t, result.Contract.MissingSince)

	// What the registry knew about the contract is kept: it is still the
	// best description of a contract that is no longer there.
	current, err := db.GetSpec(context.Background(), "testnet", idA, "")
	require.NoError(t, err)
	assert.Equal(t, "hello", current.Interface.Functions[0].Name)
}

// TestRefreshClearsTheMissingMark proves a contract that comes back is
// marked live again.
func TestRefreshClearsTheMissingMark(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	fake.Remove(idA)
	_, err = reg.Refresh(context.Background(), idA)
	require.NoError(t, err)

	got, err := db.GetContract(context.Background(), "testnet", idA)
	require.NoError(t, err)
	require.NotNil(t, got.MissingSince)

	fake.Deploy(idA, moduleWith(t, "hello"))
	_, err = reg.Refresh(context.Background(), idA)
	require.NoError(t, err)

	got, err = db.GetContract(context.Background(), "testnet", idA)
	require.NoError(t, err)
	assert.Nil(t, got.MissingSince, "a contract that is live again is not missing")
}

// TestRefreshDoesNotMarkOnRPCFailure proves that only a contract that is
// really gone is marked: an RPC failure says nothing about the contract.
func TestRefreshDoesNotMarkOnRPCFailure(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	fake.HashErr = errors.New("rpc down")

	_, err = reg.Refresh(context.Background(), idA)
	require.Error(t, err)

	got, err := db.GetContract(context.Background(), "testnet", idA)
	require.NoError(t, err)
	assert.Nil(t, got.MissingSince, "an RPC error must not mark the contract missing")
}

// TestRefreshReportsThePreviousHash proves the result says what the
// contract ran before, which is what a webhook has to report.
func TestRefreshReportsThePreviousHash(t *testing.T) {
	t.Parallel()

	reg, fake, _ := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "v1"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	fake.Upgrade(idA, moduleWith(t, "v2"))

	result, err := reg.Refresh(context.Background(), idA)
	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Equal(t, stellartest.HashOf(moduleWith(t, "v1")), result.PreviousWasmHash)
	assert.Equal(t, stellartest.HashOf(moduleWith(t, "v2")), result.Contract.CurrentWasmHash)
}

func TestRefreshUnregisteredContract(t *testing.T) {
	t.Parallel()

	reg, fake, _ := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	// Refresh works from what is stored, so a contract that was never
	// registered is a store miss rather than a network lookup.
	_, err := reg.Refresh(context.Background(), idA)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestRefreshKeepsStoredSpecWhenUpgradeFailsToDecode(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "v1"))

	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	// The contract upgrades to a module SoroVault cannot decode.
	fake.Upgrade(idA, wasmtest.Module(wasmtest.Section{Name: "producers", Body: []byte("x")}))

	_, err = reg.Refresh(context.Background(), idA)
	require.ErrorIs(t, err, spec.ErrNoSpecSection)

	// The last interface that did decode must still be served.
	current, err := db.GetSpec(context.Background(), "testnet", idA, "")
	require.NoError(t, err)
	assert.Equal(t, "v1", current.Interface.Functions[0].Name)
}

func TestNetworkMismatchIsRefused(t *testing.T) {
	t.Parallel()

	fake := stellartest.New()
	fake.Passphrase = stellar.NetworkPublic
	fake.Deploy(idA, moduleWith(t, "hello"))

	reg := registry.New(fake, spec.XDRDecoder{}, store.NewMemory(), registry.Options{
		ExpectPassphrase: stellar.NetworkTestnet,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	_, err := reg.Register(context.Background(), idA)
	require.ErrorIs(t, err, registry.ErrNetworkMismatch)
	assert.Contains(t, err.Error(), "Public Global Stellar Network")
}

func TestNetworkIsCached(t *testing.T) {
	t.Parallel()

	reg, fake, _ := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "hello"))

	for range 3 {
		_, err := reg.Register(context.Background(), idA)
		require.NoError(t, err)
	}

	assert.Equal(t, 1, fake.Calls["Network"],
		"the network is fixed for the process, so it should be resolved once")
}

// TestNetworkFailureIsNotCached checks that a transient RPC failure does not
// wedge the process into permanently refusing to resolve its network.
func TestNetworkFailureIsNotCached(t *testing.T) {
	t.Parallel()

	fake := stellartest.New()
	fake.NetworkErr = errors.New("connection refused")
	fake.Deploy(idA, moduleWith(t, "hello"))

	reg := registry.New(fake, spec.XDRDecoder{}, store.NewMemory(), registry.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	_, err := reg.Network(context.Background())
	require.Error(t, err)

	fake.NetworkErr = nil

	got, err := reg.Network(context.Background())
	require.NoError(t, err, "the registry must recover once RPC comes back")
	assert.Equal(t, "testnet", got)
}

// TestNetworkMismatchIsCached is the counterpart: a misconfiguration cannot
// be fixed by retrying, so it should not be re-queried on every call.
func TestNetworkMismatchIsCached(t *testing.T) {
	t.Parallel()

	fake := stellartest.New()
	fake.Passphrase = stellar.NetworkPublic

	reg := registry.New(fake, spec.XDRDecoder{}, store.NewMemory(), registry.Options{
		ExpectPassphrase: stellar.NetworkTestnet,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	for range 3 {
		_, err := reg.Network(context.Background())
		require.ErrorIs(t, err, registry.ErrNetworkMismatch)
	}
	assert.Equal(t, 1, fake.Calls["Network"])
}

// TestNetworkIsConcurrencySafe exercises the cached resolution under load,
// where the race detector would catch unsynchronised access.
func TestNetworkIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	reg, _, _ := newRegistry(t)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := reg.Network(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, "testnet", got)
		}()
	}
	wg.Wait()
}

func TestRegisterMultipleContracts(t *testing.T) {
	t.Parallel()

	reg, fake, db := newRegistry(t)
	fake.Deploy(idA, moduleWith(t, "a"))
	fake.Deploy(idB, moduleWith(t, "b"))

	for _, id := range []string{idA, idB} {
		_, err := reg.Register(context.Background(), id)
		require.NoError(t, err)
	}

	page, err := db.ListContracts(context.Background(), store.ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, 2, page.Total)
}

func TestRegisterDecodesRealFixture(t *testing.T) {
	t.Parallel()

	// The same contract the spec package decodes, driven through the full
	// register path to prove the layers compose.
	module, err := readFixture()
	require.NoError(t, err)

	reg, fake, _ := newRegistry(t)
	fake.Deploy(idA, module)

	result, err := reg.Register(context.Background(), idA)
	require.NoError(t, err)

	assert.Len(t, result.Interface.Functions, 7)
	assert.Equal(t,
		"c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781",
		result.Contract.CurrentWasmHash,
		"the fake keys modules by SHA-256, exactly as the network does")

	fn, ok := result.Interface.Function("has_voted")
	require.True(t, ok)
	assert.Equal(t, "fn has_voted(nullifier: BytesN<32>) -> bool", fn.Signature())
}

// TestStoreAccessor confirms the read-only handle the API and UI use.
func TestStoreAccessor(t *testing.T) {
	t.Parallel()

	reg, _, db := newRegistry(t)
	assert.Same(t, db, reg.Store())
}

// stubDecoder lets a test drive decoder failures that no real module causes.
type stubDecoder struct{ err error }

func (d stubDecoder) Decode([]byte) (*model.Interface, error) { return nil, d.err }

func TestDecoderIsPluggable(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("decoder exploded")

	fake := stellartest.New()
	fake.Deploy(idA, []byte("anything, the decoder never looks"))

	reg := registry.New(fake, stubDecoder{err: sentinel}, store.NewMemory(), registry.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	_, err := reg.Register(context.Background(), idA)
	require.ErrorIs(t, err, sentinel, "the decoder is an interface so it can be swapped wholesale")
}

// readFixture loads the checked-in testnet contract from the spec package's
// testdata, which is the project's single real-WASM fixture.
func readFixture() ([]byte, error) {
	return os.ReadFile("../spec/testdata/zkvote.wasm")
}
