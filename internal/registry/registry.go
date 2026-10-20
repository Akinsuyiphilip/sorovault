// Package registry joins the three moving parts of SoroVault: it fetches a
// contract's code from a network, decodes its interface, and records the
// result.
//
// It holds the rules that do not belong to any one of those layers — how a
// network is resolved and verified, when a refresh counts as a change, and
// what "already registered" means — so that the API, the CLI and the browse
// UI all behave identically.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
)

const (
	// DefaultMaxConcurrent is the registration concurrency limit when none is configured.
	DefaultMaxConcurrent = 4
	// DefaultAcquireTimeout is how long a registration waits for an available slot.
	DefaultAcquireTimeout = 2 * time.Second
)

var ErrBusy = errors.New("registry: too many registrations in flight")

// ErrNetworkMismatch reports that the RPC endpoint serves a different
// network than the one configured. Continuing would file records under the
// wrong namespace, so registration stops instead.
var ErrNetworkMismatch = errors.New("registry: RPC endpoint serves a different network than configured")

// ErrInvalidWasmHash reports a hash that is not a 32-byte hexadecimal value.
var ErrInvalidWasmHash = errors.New("registry: invalid wasm hash")

// Registry registers and refreshes contracts.
type Registry struct {
	fetcher stellar.Fetcher
	decoder spec.Decoder
	store   store.Store
	log     *slog.Logger

	// expectPassphrase, when set, must match what the endpoint reports.
	expectPassphrase string

	// sem bounds how many registrations and refreshes run at once, and
	// acquireTimeout bounds how long one waits for a slot. The budget is
	// shared by everything that registers — API clients, the CLI and the
	// refresher alike — because they all draw on the same memory and the
	// same network.
	sem            *semaphore.Weighted
	acquireTimeout time.Duration
	maxConcurrent  int

	// The network label is resolved from the endpoint on first use and
	// cached; it does not change for the life of the process. A mismatch is
	// cached too, since retrying cannot fix a misconfiguration, but a
	// transient RPC failure is not.
	networkMu       sync.Mutex
	network         string
	networkMismatch error
}

// Options configures a Registry.
type Options struct {
	// ExpectPassphrase is the network the operator believes RPC serves. If
	// non-empty and the endpoint disagrees, calls fail with
	// ErrNetworkMismatch rather than storing under the wrong network.
	ExpectPassphrase string
	// Logger receives operational detail. Defaults to slog.Default.
	Logger *slog.Logger
	// MaxConcurrent bounds how many registrations and refreshes run at
	// once. Values below one mean DefaultMaxConcurrent.
	MaxConcurrent int
	// AcquireTimeout bounds how long an operation waits for a slot before
	// it is refused with ErrBusy. Values below one mean
	// DefaultAcquireTimeout.
	AcquireTimeout time.Duration
}

// New builds a Registry from its three collaborators.
func New(f stellar.Fetcher, d spec.Decoder, s store.Store, opts Options) *Registry {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent < 1 {
		maxConcurrent = DefaultMaxConcurrent
	}
	acquireTimeout := opts.AcquireTimeout
	if acquireTimeout <= 0 {
		acquireTimeout = DefaultAcquireTimeout
	}
	return &Registry{
		fetcher:          f,
		decoder:          d,
		store:            s,
		log:              log,
		expectPassphrase: opts.ExpectPassphrase,
		sem:              semaphore.NewWeighted(int64(maxConcurrent)),
		acquireTimeout:   acquireTimeout,
		maxConcurrent:    maxConcurrent,
	}
}

// MaxConcurrent reports how many registrations and refreshes may run at once.
func (r *Registry) MaxConcurrent() int { return r.maxConcurrent }

// AcquireTimeout reports how long an operation waits for a slot before it
// is refused with ErrBusy.
func (r *Registry) AcquireTimeout() time.Duration { return r.acquireTimeout }

// Result describes what a register or refresh call did.
type Result struct {
	Contract  store.Contract   `json:"contract"`
	Interface *model.Interface `json:"interface,omitempty"`

	// Created is true when this call added a contract the registry had not
	// seen before.
	Created bool `json:"created"`
	// Changed is true when the contract's WASM hash differed from what was
	// stored, so a new interface version was recorded.
	Changed bool `json:"changed"`
	// PreviousWasmHash is the hash the contract ran before, when this call
	// changed it. It is empty when nothing changed.
	PreviousWasmHash string `json:"previous_wasm_hash,omitempty"`
	// Missing is true when the contract could not be found on the network.
	// The call succeeded — the contract's record is kept, and marked as
	// missing — so this is not an error.
	Missing bool `json:"missing,omitempty"`
}

// WasmResult describes a catalogued upload that may not be deployed yet.
type WasmResult struct {
	Network   string           `json:"network"`
	WasmHash  string           `json:"wasm_hash"`
	Interface *model.Interface `json:"interface"`
	DecodedAt time.Time        `json:"decoded_at"`
	Created   bool             `json:"created"`
}

// Network returns the label records are stored under, resolved from the RPC
// endpoint on first call and cached thereafter.
func (r *Registry) Network(ctx context.Context) (string, error) {
	r.networkMu.Lock()
	defer r.networkMu.Unlock()

	if r.networkMismatch != nil {
		return "", r.networkMismatch
	}
	if r.network != "" {
		return r.network, nil
	}

	passphrase, err := r.fetcher.Network(ctx)
	if err != nil {
		// Left uncached: a blip on the first call must not wedge the
		// process into permanently refusing to resolve its network.
		return "", err
	}

	if err := CheckPassphrase(passphrase, r.expectPassphrase); err != nil {
		r.networkMismatch = err
		return "", r.networkMismatch
	}

	r.network = stellar.NetworkName(passphrase)
	return r.network, nil
}

// CheckPassphrase reports whether the passphrase an endpoint reports is the
// one the operator expects.
//
// An empty expectation accepts whatever the endpoint says: not every
// deployment knows the passphrase it is talking to, and refusing to work
// because of that would be worse than the risk.
func CheckPassphrase(got, want string) error {
	if want == "" || got == want {
		return nil
	}
	return fmt.Errorf("%w: endpoint reports %q, configured %q",
		ErrNetworkMismatch, got, want)
}

// acquire takes a slot in the concurrency budget, waiting at most for the
// configured window.
//
// A deadline that expires is the registry's, not the caller's: the caller
// still wants the work done and the registry is simply too busy, which is
// ErrBusy. A cancelled caller context is the caller's own, and is returned
// as it is.
func (r *Registry) acquire(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, r.acquireTimeout)
	defer cancel()

	if err := r.sem.Acquire(waitCtx, 1); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("%w: %d already running", ErrBusy, r.maxConcurrent)
		}
		return err
	}
	return nil
}

// release gives a slot back. It is called once per successful acquire, on
// every path out of an operation: success, failure and cancellation alike.
func (r *Registry) release() {
	r.sem.Release(1)
}

// Register fetches, decodes and stores a contract's interface.
//
// It is safe to call on a contract that is already registered: an unchanged
// contract is simply re-verified and its last_refreshed bumped, which makes
// the operation idempotent and lets it double as an explicit refresh.
func (r *Registry) Register(ctx context.Context, contractID string) (*Result, error) {
	if err := stellar.ValidateContractID(contractID); err != nil {
		return nil, err
	}

	// The budget is taken before anything expensive happens, and given back
	// on every path out of here.
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()

	network, err := r.Network(ctx)
	if err != nil {
		return nil, err
	}

	// Whether the contract is new decides Created, and it has to be read
	// before Save overwrites the answer.
	existing, err := r.store.GetContract(ctx, network, contractID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	contract, err := r.fetcher.FetchContract(ctx, contractID)
	if err != nil {
		return nil, err
	}

	iface, err := r.decoder.Decode(contract.Wasm)
	if err != nil {
		return nil, fmt.Errorf("registry: decoding %s: %w", contractID, err)
	}

	record := store.Contract{
		ContractID:      contractID,
		Network:         network,
		CurrentWasmHash: contract.WasmHash,
	}
	if err := r.store.Save(ctx, record, iface); err != nil {
		return nil, err
	}

	saved, err := r.store.GetContract(ctx, network, contractID)
	if err != nil {
		return nil, err
	}

	result := &Result{
		Contract:  *saved,
		Interface: iface,
		Created:   existing == nil,
		Changed:   existing == nil || existing.CurrentWasmHash != contract.WasmHash,
	}
	if result.Changed && existing != nil {
		result.PreviousWasmHash = existing.CurrentWasmHash
	}

	r.log.InfoContext(ctx, "registered contract",
		"contract_id", contractID,
		"network", network,
		"wasm_hash", contract.WasmHash,
		"created", result.Created,
		"changed", result.Changed,
		"functions", len(iface.Functions),
	)

	return result, nil
}

// CatalogWasm decodes code uploaded to the network without requiring a
// contract instance to reference it.
func (r *Registry) CatalogWasm(ctx context.Context, wasmHash string) (*WasmResult, error) {
	rawHash, err := hex.DecodeString(wasmHash)
	if err != nil || len(rawHash) != sha256.Size {
		return nil, fmt.Errorf("%w: %q", ErrInvalidWasmHash, wasmHash)
	}
	wasmHash = hex.EncodeToString(rawHash)

	network, err := r.Network(ctx)
	if err != nil {
		return nil, err
	}
	if existing, err := r.store.GetWasmSpec(ctx, network, wasmHash); err == nil {
		return &WasmResult{
			Network: existing.Network, WasmHash: existing.WasmHash,
			Interface: existing.Interface, DecodedAt: existing.DecodedAt,
		}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	wasm, err := r.fetcher.FetchWasm(ctx, wasmHash)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(wasm)
	if hex.EncodeToString(digest[:]) != wasmHash {
		return nil, fmt.Errorf("registry: fetched wasm does not match hash %s", wasmHash)
	}

	iface, err := r.decoder.Decode(wasm)
	if err != nil {
		return nil, fmt.Errorf("registry: decoding wasm %s: %w", wasmHash, err)
	}
	if err := r.store.SaveWasmSpec(ctx, network, wasmHash, iface); err != nil {
		return nil, err
	}
	stored, err := r.store.GetWasmSpec(ctx, network, wasmHash)
	if err != nil {
		return nil, err
	}
	return &WasmResult{
		Network: stored.Network, WasmHash: stored.WasmHash,
		Interface: stored.Interface, DecodedAt: stored.DecodedAt, Created: true,
	}, nil
}

// Refresh re-checks a registered contract against the network and records a
// new interface version if its WASM has changed.
//
// Unlike Register it starts from what is already stored, so it can settle an
// unchanged contract with a single RPC round trip: the module is only
// downloaded once the hash is known to differ.
//
// A contract that is no longer on the network is not an error: it is marked
// missing, and the result says so.
func (r *Registry) Refresh(ctx context.Context, contractID string) (*Result, error) {
	if err := stellar.ValidateContractID(contractID); err != nil {
		return nil, err
	}

	// Refreshes draw on the same budget as registrations: both download and
	// decode modules, and the refresher's own sweep is no more entitled to
	// the memory than a client asking over HTTP.
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()

	network, err := r.Network(ctx)
	if err != nil {
		return nil, err
	}

	existing, err := r.store.GetContract(ctx, network, contractID)
	if err != nil {
		return nil, err
	}

	currentHash, err := r.fetcher.WasmHash(ctx, contractID)
	if err != nil {
		if errors.Is(err, stellar.ErrContractNotFound) {
			return r.markMissing(ctx, network, existing)
		}
		return nil, err
	}

	if currentHash == existing.CurrentWasmHash {
		// Nothing new to decode, but the contract was just confirmed live,
		// so re-save to advance last_refreshed. Save is a no-op on the spec
		// row when the hash is unchanged, and clears any missing mark.
		stored, err := r.store.GetSpec(ctx, network, contractID, currentHash)
		if err != nil {
			return nil, err
		}
		if err := r.store.Save(ctx, *existing, stored.Interface); err != nil {
			return nil, err
		}
		updated, err := r.store.GetContract(ctx, network, contractID)
		if err != nil {
			return nil, err
		}

		r.log.DebugContext(ctx, "contract unchanged",
			"contract_id", contractID, "network", network, "wasm_hash", currentHash)

		return &Result{Contract: *updated, Interface: stored.Interface}, nil
	}

	contract, err := r.fetcher.FetchContract(ctx, contractID)
	if err != nil {
		if errors.Is(err, stellar.ErrContractNotFound) {
			return r.markMissing(ctx, network, existing)
		}
		return nil, err
	}

	iface, err := r.decoder.Decode(contract.Wasm)
	if err != nil {
		return nil, fmt.Errorf("registry: decoding %s: %w", contractID, err)
	}

	record := *existing
	record.CurrentWasmHash = contract.WasmHash
	if err := r.store.Save(ctx, record, iface); err != nil {
		return nil, err
	}

	updated, err := r.store.GetContract(ctx, network, contractID)
	if err != nil {
		return nil, err
	}

	r.log.InfoContext(ctx, "contract upgraded",
		"contract_id", contractID,
		"network", network,
		"from_wasm_hash", existing.CurrentWasmHash,
		"to_wasm_hash", contract.WasmHash,
	)

	return &Result{
		Contract:         *updated,
		Interface:        iface,
		Changed:          true,
		PreviousWasmHash: existing.CurrentWasmHash,
	}, nil
}

// markMissing records that a contract has gone from the network.
//
// The contract's record is kept: its last interface is still the best
// description of what the contract was, and the mark says when it stopped
// being live. Only a contract that is genuinely absent is marked — an RPC
// failure says nothing about the contract.
func (r *Registry) markMissing(ctx context.Context, network string, existing *store.Contract) (*Result, error) {
	if err := r.store.MarkMissing(ctx, network, existing.ContractID, time.Now().UTC()); err != nil {
		return nil, err
	}
	updated, err := r.store.GetContract(ctx, network, existing.ContractID)
	if err != nil {
		return nil, err
	}

	// Debug, not a warning: the sweep reports a contract that has just
	// gone missing, and repeating that on every sweep would drown it.
	r.log.DebugContext(ctx, "contract not found on the network",
		"contract_id", existing.ContractID, "network", network,
		"missing_since", updated.MissingSince)

	return &Result{Contract: *updated, Missing: true}, nil
}

// Store exposes the underlying store for read-only queries the API and UI
// serve directly, so they do not each need their own handle.
func (r *Registry) Store() store.Store { return r.store }
