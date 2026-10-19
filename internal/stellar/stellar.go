// Package stellar fetches deployed contract code from a Stellar network.
//
// Reading a contract's WASM takes two RPC round trips. The contract's
// instance ledger entry holds an executable that names the WASM by hash;
// that hash is then the key of the contract code entry holding the bytes
// themselves. Callers that only need to know whether a contract has changed
// can stop after the first trip by calling WasmHash.
package stellar

import (
	"context"
	"errors"
	"fmt"
)

// ErrContractNotFound reports that the network has no live instance entry
// for a contract ID. That covers both a contract that was never deployed and
// one whose entry has expired and not been restored.
var ErrContractNotFound = errors.New("stellar: contract not found on network")

// ErrNoWasm reports that a contract exists but has no WASM to fetch. Stellar
// asset contracts are the usual case: they are built into the host rather
// than uploaded, so they have no interface to decode.
var ErrNoWasm = errors.New("stellar: contract has no uploaded wasm")

// ErrWasmNotFound reports that the network has no uploaded code for a hash.
var ErrWasmNotFound = errors.New("stellar: wasm not found on network")

// Contract is a deployed contract's code as fetched from the network.
type Contract struct {
	// ID is the contract's strkey address ("C...").
	ID string
	// WasmHash is the lowercase hex SHA-256 of the WASM, which is also the
	// key its code entry is stored under.
	WasmHash string
	// Wasm is the uploaded module.
	Wasm []byte
}

// Fetcher reads deployed contract code from a network.
//
// It is the seam the rest of SoroVault is written against: the registry
// depends on this interface, never on an RPC client, so tests can supply a
// fixture-backed implementation and never touch the network.
type Fetcher interface {
	// FetchContract returns the contract's WASM and the hash it is keyed by.
	FetchContract(ctx context.Context, contractID string) (*Contract, error)

	// FetchWasm returns uploaded code by its SHA-256 hash, whether or not a
	// contract instance currently references it.
	FetchWasm(ctx context.Context, wasmHash string) ([]byte, error)

	// WasmHash returns just the hash the contract currently points at,
	// without downloading the module. Refresh uses it to decide whether
	// there is anything new to decode.
	WasmHash(ctx context.Context, contractID string) (string, error)

	// Network returns the passphrase of the network this fetcher reads,
	// which the registry uses to namespace what it stores.
	Network(ctx context.Context) (string, error)
}

// NetworkName maps a network passphrase to the short label SoroVault stores
// records under. An unrecognised passphrase is returned unchanged so that
// private and future networks still get a stable, distinct namespace.
func NetworkName(passphrase string) string {
	switch passphrase {
	case NetworkPublic:
		return "public"
	case NetworkTestnet:
		return "testnet"
	case NetworkFuturenet:
		return "futurenet"
	case "":
		return "unknown"
	default:
		return passphrase
	}
}

// Well-known network passphrases.
const (
	NetworkPublic    = "Public Global Stellar Network ; September 2015"
	NetworkTestnet   = "Test SDF Network ; September 2015"
	NetworkFuturenet = "Test SDF Future Network ; October 2022"
)

// notFoundError wraps ErrContractNotFound with the ID that was missing.
func notFoundError(contractID string) error {
	return fmt.Errorf("%w: %s", ErrContractNotFound, contractID)
}
