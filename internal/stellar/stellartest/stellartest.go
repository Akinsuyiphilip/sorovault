// Package stellartest provides a fake network for tests.
//
// Everything above internal/stellar depends on the Fetcher interface rather
// than on RPC, so a fake implementation lets the registry, API and browse UI
// be tested end to end without a network — which is what keeps
// `go test ./...` self-contained.
package stellartest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/soroworks/sorovault/internal/stellar"
)

// Fake is an in-memory stellar.Fetcher.
//
// Contracts are added with Deploy and replaced with Upgrade, mirroring how a
// contract's WASM changes on chain, so a test can exercise refresh without
// hand-managing hashes.
type Fake struct {
	mu sync.Mutex

	contracts map[string][]byte
	wasm      map[string][]byte

	// Passphrase is what Network reports. It defaults to testnet.
	Passphrase string

	// FetchErr and HashErr, when set, are returned by the corresponding
	// method instead of a result.
	FetchErr error
	HashErr  error
	// NetworkErr, when set, is returned by Network.
	NetworkErr error

	// BeforeFetch, when set, runs before every fetch and hash lookup, and
	// may block. Holding a fetch open is how a test proves a concurrency
	// bound: the calls that got a slot are inside it, and the ones that did
	// not never reach it.
	BeforeFetch func(ctx context.Context, contractID string) error

	// Calls counts invocations by method name, so a test can assert that
	// refreshing an unchanged contract did not download its module.
	Calls map[string]int
}

var _ stellar.Fetcher = (*Fake)(nil)

// New returns an empty fake network reporting the testnet passphrase.
func New() *Fake {
	return &Fake{
		contracts:  map[string][]byte{},
		wasm:       map[string][]byte{},
		Passphrase: stellar.NetworkTestnet,
		Calls:      map[string]int{},
	}
}

// UploadWasm makes code available by hash without creating a contract instance.
func (f *Fake) UploadWasm(wasmModule []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash := HashOf(wasmModule)
	f.wasm[hash] = append([]byte(nil), wasmModule...)
	return hash
}

// Deploy makes contractID resolve to the given module.
func (f *Fake) Deploy(contractID string, wasmModule []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contracts[contractID] = wasmModule
}

// Upgrade replaces a contract's module, as an on-chain upgrade would. The
// new module must differ from the old one, or the hash will not change and
// the upgrade will be invisible to a refresh.
func (f *Fake) Upgrade(contractID string, wasmModule []byte) {
	f.Deploy(contractID, wasmModule)
}

// Remove makes contractID stop resolving, as an expired entry would.
func (f *Fake) Remove(contractID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.contracts, contractID)
}

// FetchContract implements stellar.Fetcher.
func (f *Fake) FetchContract(ctx context.Context, contractID string) (*stellar.Contract, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := stellar.ValidateContractID(contractID); err != nil {
		return nil, err
	}
	// The hook runs before the lock is taken, so a test can hold a fetch
	// open without freezing the whole fake.
	if err := f.beforeFetch(ctx, contractID); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["FetchContract"]++

	if f.FetchErr != nil {
		return nil, f.FetchErr
	}

	module, ok := f.contracts[contractID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", stellar.ErrContractNotFound, contractID)
	}

	return &stellar.Contract{
		ID:       contractID,
		WasmHash: HashOf(module),
		Wasm:     module,
	}, nil
}

// FetchWasm implements stellar.Fetcher.
func (f *Fake) FetchWasm(ctx context.Context, wasmHash string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["FetchWasm"]++
	module, ok := f.wasm[wasmHash]
	if !ok {
		for _, deployed := range f.contracts {
			if HashOf(deployed) == wasmHash {
				module = deployed
				ok = true
				break
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", stellar.ErrWasmNotFound, wasmHash)
	}
	return append([]byte(nil), module...), nil
}

// WasmHash implements stellar.Fetcher.
func (f *Fake) WasmHash(ctx context.Context, contractID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := stellar.ValidateContractID(contractID); err != nil {
		return "", err
	}
	if err := f.beforeFetch(ctx, contractID); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["WasmHash"]++

	if f.HashErr != nil {
		return "", f.HashErr
	}

	module, ok := f.contracts[contractID]
	if !ok {
		return "", fmt.Errorf("%w: %s", stellar.ErrContractNotFound, contractID)
	}
	return HashOf(module), nil
}

// beforeFetch runs the BeforeFetch hook, if the test set one.
func (f *Fake) beforeFetch(ctx context.Context, contractID string) error {
	f.mu.Lock()
	hook := f.BeforeFetch
	f.mu.Unlock()

	if hook == nil {
		return nil
	}
	return hook(ctx, contractID)
}

// Network implements stellar.Fetcher.
func (f *Fake) Network(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["Network"]++

	if f.NetworkErr != nil {
		return "", f.NetworkErr
	}
	return f.Passphrase, nil
}

// HashOf returns a module's WASM hash. A contract's hash really is the
// SHA-256 of its module, so the fake keys code exactly as the network does.
func HashOf(wasmModule []byte) string {
	sum := sha256.Sum256(wasmModule)
	return hex.EncodeToString(sum[:])
}
