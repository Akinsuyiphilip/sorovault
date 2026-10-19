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

// ContractPage is one page of a contract listing.
type ContractPage struct {
	Contracts []Contract `json:"contracts"`
	// Total is how many contracts match the filter, ignoring paging.
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

	// ListContracts returns a filtered, paged listing.
	ListContracts(ctx context.Context, f ListFilter) (*ContractPage, error)

	// ListNetworks returns the distinct networks represented in the registry,
	// in ascending order.
	ListNetworks(ctx context.Context) ([]string, error)

	// GetSpec returns a contract's decoded interface. An empty wasmHash
	// selects whichever version the contract currently runs.
	GetSpec(ctx context.Context, network, contractID, wasmHash string) (*Spec, error)

	// ListVersions returns every stored version of a contract, newest first.
	ListVersions(ctx context.Context, network, contractID string) ([]SpecVersion, error)

	// Close releases any resources held by the store.
	Close() error
}
