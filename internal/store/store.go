// Package store persists the contract registry.
//
// The registry keeps two things per contract: a row tracking which WASM the
// contract currently runs, and one immutable spec row per WASM hash it has
// ever run. Refreshing a contract therefore appends a version rather than
// overwriting one, so an interface that was live at some point stays
// retrievable after the contract is upgraded.
package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/soroworks/sorovault/internal/model"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// Contract is a registered contract's current state.
type Contract struct {
	ContractID      string    `json:"contract_id"`
	Network         string    `json:"network"`
	CurrentWasmHash string    `json:"current_wasm_hash"`
	Name            string    `json:"name,omitempty"`
	SourceURL       string    `json:"source_url,omitempty"`
	SourceCommit    string    `json:"source_commit,omitempty"`
	FirstSeen       time.Time `json:"first_seen"`
	LastRefreshed   time.Time `json:"last_refreshed"`

	// MissingSince is when a refresh first found the contract absent from
	// the network. It is nil while the contract is live, and is cleared by
	// the next save, which only happens once the contract has been fetched
	// again.
	MissingSince *time.Time `json:"missing_since,omitempty"`

	// Matches lists the interface symbols — functions, types, events — that
	// a search query matched. It is only set on search results, and is
	// empty when the query matched the contract ID or name instead.
	Matches []string `json:"matches,omitempty"`
}

// symbolSeparator joins symbol names into the single searchable string the
// Postgres store keeps per spec. A newline cannot appear in a symbol, and a
// query cannot contain one once trimmed, so a substring match can never
// straddle two names.
const symbolSeparator = "\n"

// joinSymbols renders an interface's symbol names for storage.
func joinSymbols(iface *model.Interface) string {
	return strings.Join(iface.SymbolNames(), symbolSeparator)
}

// matchSymbols returns the names in a joined symbol string that contain
// query, compared case-insensitively. Both stores use it so they report
// identical matches.
func matchSymbols(joined, query string) []string {
	if query == "" || joined == "" {
		return nil
	}
	needle := strings.ToLower(query)
	var out []string
	for _, name := range strings.Split(joined, symbolSeparator) {
		if strings.Contains(strings.ToLower(name), needle) {
			out = append(out, name)
		}
	}
	return out
}

// hasFunction reports whether an interface declares a function with exactly
// this name.
//
// The match is exact: "transfer" is not "transfer_from" and not
// "Transferable". A caller asking for a function wants the interface to
// implement it, not merely to mention it.
func hasFunction(iface *model.Interface, name string) bool {
	if iface == nil {
		return false
	}
	for _, fn := range iface.Functions {
		if fn.Name == name {
			return true
		}
	}
	return false
}

// hasType reports whether an interface declares a user-defined type with
// exactly this name.
//
// Error enums count: they are types the contract defines, and a client
// matching on a type name means the type it will use.
func hasType(iface *model.Interface, name string) bool {
	if iface == nil {
		return false
	}
	for _, s := range iface.Types.Structs {
		if s.Name == name {
			return true
		}
	}
	for _, u := range iface.Types.Unions {
		if u.Name == name {
			return true
		}
	}
	for _, e := range iface.Types.Enums {
		if e.Name == name {
			return true
		}
	}
	for _, e := range iface.Types.ErrorEnums {
		if e.Name == name {
			return true
		}
	}
	return false
}

// hasAll reports whether an interface declares every requested function and
// type name. The in-memory store uses it; the Postgres one asks the
// database the same question.
func hasAll(iface *model.Interface, functions, types []string) bool {
	for _, name := range functions {
		if !hasFunction(iface, name) {
			return false
		}
	}
	for _, name := range types {
		if !hasType(iface, name) {
			return false
		}
	}
	return true
}

// Spec is one decoded interface version of a contract.
type Spec struct {
	ID         int64            `json:"id"`
	ContractID string           `json:"contract_id"`
	Network    string           `json:"network"`
	WasmHash   string           `json:"wasm_hash"`
	Interface  *model.Interface `json:"interface"`
	DecodedAt  time.Time        `json:"decoded_at"`
}

// WasmSpec is a decoded interface for uploaded code that no registered
// contract currently runs.
type WasmSpec struct {
	Network   string           `json:"network"`
	WasmHash  string           `json:"wasm_hash"`
	Interface *model.Interface `json:"interface"`
	DecodedAt time.Time        `json:"decoded_at"`
}

// SpecVersion summarises a stored spec without its body, for listing the
// versions of a contract cheaply.
type SpecVersion struct {
	WasmHash  string    `json:"wasm_hash"`
	DecodedAt time.Time `json:"decoded_at"`
	Current   bool      `json:"current"`
}

// ListFilter selects and pages a contract listing.
type ListFilter struct {
	// Network restricts results to one network. Empty means all networks.
	Network string
	// Query is a case-insensitive substring match against contract ID,
	// name, and the names of the functions, types and events in the
	// contract's current interface. Empty matches everything.
	Query string
	// Functions restricts results to contracts whose current interface
	// declares a function with exactly this name. Every name must match,
	// so two names ask for a contract with both.
	Functions []string
	// Types restricts results to contracts whose current interface declares
	// a user-defined type with exactly this name. Every name must match.
	Types []string
	// Limit is the maximum number of rows to return. Zero means
	// DefaultLimit; anything above MaxLimit is clamped to MaxLimit.
	Limit int
	// Offset is the number of rows to skip.
	Offset int
}

// Paging bounds for ListFilter.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Normalize trims the search term and clamps the filter's paging to the
// supported range. Implementations call it, so callers taking a filter
// straight from user input do not have to.
func (f *ListFilter) Normalize() {
	f.Query = strings.TrimSpace(f.Query)
	f.Functions = cleanNames(f.Functions)
	f.Types = cleanNames(f.Types)
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
}

// cleanNames trims a list of symbol names and drops the empty ones, so a
// repeated query parameter with no value does not filter everything out.
func cleanNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ContractPage is one page of a contract listing.
type ContractPage struct {
	Contracts []Contract `json:"contracts"`
	// Total is how many contracts match the filter, ignoring paging.
	Total int `json:"total"`
}

// WasmFilter selects the contracts that run a given WASM hash.
//
// Many contracts share one WASM — every instance of a token template, every
// vault a factory deployed — so the question "who else runs this code?" is
// the one a vulnerability in a shared WASM raises.
type WasmFilter struct {
	// Network restricts results to one network. Empty means all networks.
	Network string
	// WasmHash is the hash of the WASM the contracts run.
	WasmHash string
	// Ever includes the contracts that ran the hash in the past, not only
	// the ones running it now.
	Ever bool
	// Limit is the maximum number of rows to return. Zero means
	// DefaultLimit; anything above MaxLimit is clamped to MaxLimit.
	Limit int
	// Offset is the number of rows to skip.
	Offset int
}

// NetworkStats is one network's share of the registry.
type NetworkStats struct {
	Network   string `json:"network"`
	Contracts int    `json:"contracts"`
	Versions  int    `json:"versions"`
}

// Stats summarises the size of the registry.
type Stats struct {
	// Networks is every network with a registered contract, ordered by
	// name.
	Networks []NetworkStats `json:"networks"`
	// Contracts and Versions total every network.
	Contracts int `json:"contracts"`
	Versions  int `json:"versions"`
	// Latest is the most recently registered contract, if the registry has
	// one. Registered means first seen: refreshing an old contract does not
	// make it new.
	Latest *Contract `json:"latest_contract,omitempty"`
}

// Upgrade is one recorded interface upgrade: the version a contract ran
// before, the version it moved to, and when the new one was observed.
//
// It is derived from the specs rows rather than stored separately: any spec
// row beyond a contract's earliest is, by definition, an upgrade.
type Upgrade struct {
	ContractID  string    `json:"contract_id"`
	Network     string    `json:"network"`
	OldWasmHash string    `json:"old_wasm_hash"`
	NewWasmHash string    `json:"new_wasm_hash"`
	ObservedAt  time.Time `json:"observed_at"`
}

// UpgradeFilter selects and pages the upgrade feed.
type UpgradeFilter struct {
	// Network restricts results to one network. Empty means all networks.
	Network string
	// Limit is the maximum number of upgrades to return. Zero means
	// DefaultLimit; anything above MaxLimit is clamped to MaxLimit.
	Limit int
	// Offset is the number of upgrades to skip.
	Offset int
}

// Normalize trims the network and clamps the filter's paging to the
// supported range, as ListFilter.Normalize does.
func (f *WasmFilter) Normalize() {
	f.Network = strings.TrimSpace(f.Network)
	f.WasmHash = strings.TrimSpace(f.WasmHash)
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
}

// Normalize trims the network and clamps the filter's paging to the
// supported range.
func (f *UpgradeFilter) Normalize() {
	f.Network = strings.TrimSpace(f.Network)
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
}

// WasmContract is one contract in a listing of the contracts that run a
// given WASM hash.
type WasmContract struct {
	Contract

	// Current is true when the contract runs the hash now, and false when
	// it only ran it in the past.
	Current bool `json:"current"`
}

// WasmContractPage is one page of a listing of the contracts that run a
// given WASM hash.
type WasmContractPage struct {
	Contracts []WasmContract `json:"contracts"`
	// Total is how many contracts match, ignoring paging.
	Total int `json:"total"`
}

// UpgradePage is one page of the upgrade feed.
type UpgradePage struct {
	Upgrades []Upgrade `json:"upgrades"`
	// Total is how many upgrades match the filter, ignoring paging.
	Total int `json:"total"`
}

// Store is the registry's persistence layer.
//
// Implementations must be safe for concurrent use. Two are provided: Postgres
// for real deployments, and an in-memory one used by tests.
type Store interface {
	// UpdateSource attaches repository metadata to an existing contract.
	UpdateSource(ctx context.Context, network, contractID, sourceURL, sourceCommit string) error

	// SaveWasmSpec records an interface by network and WASM hash, independent
	// of any deployed contract.
	SaveWasmSpec(ctx context.Context, network, wasmHash string, iface *model.Interface) error

	// GetWasmSpec returns an interface for uploaded code by its hash.
	GetWasmSpec(ctx context.Context, network, wasmHash string) (*WasmSpec, error)

	// Save records a decoded interface and points the contract at it.
	//
	// It is idempotent per WASM hash: re-saving a hash already stored for a
	// contract updates the contract's last_refreshed without inserting a
	// duplicate spec row. Saving a new hash appends a version and leaves
	// earlier ones intact.
	Save(ctx context.Context, c Contract, iface *model.Interface) error

	// GetContract returns a contract's current state.
	GetContract(ctx context.Context, network, contractID string) (*Contract, error)

	// MarkMissing records that a contract could not be found on the
	// network, and when it was first noticed to be gone.
	//
	// The mark is not overwritten once it is set: the timestamp says when
	// the contract disappeared, not when it was last looked for. Saving
	// the contract again clears it, since a stored interface means the
	// contract was live.
	MarkMissing(ctx context.Context, network, contractID string, since time.Time) error

	// ListContracts returns a filtered, paged listing.
	ListContracts(ctx context.Context, f ListFilter) (*ContractPage, error)

	// ListContractsByWasm returns a paged listing of the contracts that
	// run the given WASM hash.
	//
	// With Ever set, the listing also covers the contracts that ran the
	// hash in the past and have since been upgraded; each result says
	// whether the contract still runs it.
	ListContractsByWasm(ctx context.Context, f WasmFilter) (*WasmContractPage, error)

	// ListNetworks returns the distinct networks represented in the registry,
	// in ascending order.
	ListNetworks(ctx context.Context) ([]string, error)

	// Stats summarises the registry: contract and version counts per
	// network, and the most recently registered contract.
	Stats(ctx context.Context) (*Stats, error)

	// ListUpgrades returns recent interface upgrades, newest first. A
	// contract's first stored version is its registration, not an upgrade,
	// so it is excluded.
	ListUpgrades(ctx context.Context, f UpgradeFilter) (*UpgradePage, error)

	// GetSpec returns a contract's decoded interface. An empty wasmHash
	// selects whichever version the contract currently runs.
	GetSpec(ctx context.Context, network, contractID, wasmHash string) (*Spec, error)

	// ListVersions returns every stored version of a contract, newest first.
	ListVersions(ctx context.Context, network, contractID string) ([]SpecVersion, error)

	// Close releases any resources held by the store.
	Close() error
}
