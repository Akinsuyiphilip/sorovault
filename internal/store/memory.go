package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/soroworks/sorovault/internal/model"
)

// Memory is an in-process Store.
//
// It exists so the API, CLI and browse UI can be tested without a database,
// and so `sorovault` can be run against a throwaway registry. It is not
// durable: everything is lost when the process exits.
type Memory struct {
	mu        sync.RWMutex
	contracts map[string]Contract
	specs     map[string][]Spec
	wasmSpecs map[string]WasmSpec
	nextID    int64

	// now is the clock, swappable so tests can produce deterministic and
	// strictly ordered timestamps.
	now func() time.Time
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		contracts: make(map[string]Contract),
		specs:     make(map[string][]Spec),
		wasmSpecs: make(map[string]WasmSpec),
		now:       time.Now,
	}
}

// SetClock replaces the store's clock. Tests use it to make timestamps
// predictable; production code should leave the default alone.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Close satisfies Store. There is nothing to release.
func (m *Memory) Close() error { return nil }

func key(network, contractID string) string { return network + "\x00" + contractID }

func wasmSpecKey(network, wasmHash string) string { return network + "\x00" + wasmHash }

// SaveWasmSpec stores an interface for uploaded code independent of contracts.
func (m *Memory) SaveWasmSpec(ctx context.Context, network, wasmHash string, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cloned, err := cloneInterface(iface)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wasmSpecs[wasmSpecKey(network, wasmHash)] = WasmSpec{
		Network: network, WasmHash: wasmHash, Interface: cloned, DecodedAt: m.now(),
	}
	return nil
}

// GetWasmSpec returns a hash-addressed interface without requiring a contract.
func (m *Memory) GetWasmSpec(ctx context.Context, network, wasmHash string) (*WasmSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.wasmSpecs[wasmSpecKey(network, wasmHash)]
	if !ok {
		return nil, fmt.Errorf("%w: wasm spec %s on %s", ErrNotFound, wasmHash, network)
	}
	cloned, err := cloneInterface(s.Interface)
	if err != nil {
		return nil, err
	}
	s.Interface = cloned
	return &s, nil
}

// Save records a decoded interface and points the contract at it, matching
// the Postgres implementation's append-don't-overwrite semantics.
func (m *Memory) Save(ctx context.Context, c Contract, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Round-tripping through JSON matches what Postgres does to a spec on
	// the way in and out, so tests cannot come to depend on a shared
	// pointer that the real store would never hand back.
	cloned, err := cloneInterface(iface)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	k := key(c.Network, c.ContractID)

	existing, found := m.contracts[k]
	if found {
		c.FirstSeen = existing.FirstSeen
		if c.Name == "" {
			c.Name = existing.Name
		}
		if c.SourceURL == "" {
			c.SourceURL = existing.SourceURL
		}
		if c.SourceCommit == "" {
			c.SourceCommit = existing.SourceCommit
		}
	} else {
		c.FirstSeen = now
	}
	c.LastRefreshed = now
	// A saved interface means the contract was live, so it is not missing
	// any more.
	c.MissingSince = nil
	m.contracts[k] = c

	for i := range m.specs[k] {
		if m.specs[k][i].WasmHash == c.CurrentWasmHash {
			m.specs[k][i].Interface = cloned
			return nil
		}
	}

	m.nextID++
	m.specs[k] = append(m.specs[k], Spec{
		ID:         m.nextID,
		Network:    c.Network,
		ContractID: c.ContractID,
		WasmHash:   c.CurrentWasmHash,
		Interface:  cloned,
		DecodedAt:  now,
	})
	return nil
}

// MarkMissing records that a contract could not be found on the network.
func (m *Memory) MarkMissing(ctx context.Context, network, contractID string, since time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	k := key(network, contractID)
	c, ok := m.contracts[k]
	if !ok {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	// The first sighting is the one that matters: a contract that stays
	// missing keeps the timestamp of when it went away.
	if c.MissingSince == nil {
		stamp := since.UTC()
		c.MissingSince = &stamp
		m.contracts[k] = c
	}
	return nil
}

// UpdateSource attaches repository metadata to an existing contract.
func (m *Memory) UpdateSource(ctx context.Context, network, contractID, sourceURL, sourceCommit string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(network, contractID)
	c, ok := m.contracts[k]
	if !ok {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	c.SourceURL = sourceURL
	c.SourceCommit = sourceCommit
	m.contracts[k] = c
	return nil
}

// GetContract returns one contract's current state.
func (m *Memory) GetContract(ctx context.Context, network, contractID string) (*Contract, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	c, ok := m.contracts[key(network, contractID)]
	if !ok {
		return nil, fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	c.MissingSince = copyMissing(c.MissingSince)
	return &c, nil
}

// copyMissing returns an independent copy of a missing mark, so that a
// caller cannot change what the store holds through the pointer.
func copyMissing(since *time.Time) *time.Time {
	if since == nil {
		return nil
	}
	out := *since
	return &out
}

// ListNetworks returns the distinct networks represented in the registry.
func (m *Memory) ListNetworks(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]struct{})
	for _, contract := range m.contracts {
		seen[contract.Network] = struct{}{}
	}
	networks := make([]string, 0, len(seen))
	for network := range seen {
		networks = append(networks, network)
	}
	sort.Strings(networks)
	return networks, nil
}

// Stats summarises the registry from its in-memory rows.
func (m *Memory) Stats(ctx context.Context) (*Stats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	byNetwork := make(map[string]*NetworkStats)
	stats := &Stats{Networks: []NetworkStats{}}
	for k, c := range m.contracts {
		entry, ok := byNetwork[c.Network]
		if !ok {
			entry = &NetworkStats{Network: c.Network}
			byNetwork[c.Network] = entry
		}
		entry.Contracts++
		entry.Versions += len(m.specs[k])
		stats.Contracts++
		stats.Versions += len(m.specs[k])

		if stats.Latest == nil || newerContract(c, *stats.Latest) {
			latest := c
			latest.MissingSince = copyMissing(c.MissingSince)
			stats.Latest = &latest
		}
	}

	networks := make([]string, 0, len(byNetwork))
	for network := range byNetwork {
		networks = append(networks, network)
	}
	sort.Strings(networks)
	for _, network := range networks {
		stats.Networks = append(stats.Networks, *byNetwork[network])
	}
	return stats, nil
}

// newerContract reports whether a was registered more recently than b. The
// contract ID breaks a tie so the answer does not depend on map order.
func newerContract(a, b Contract) bool {
	if !a.FirstSeen.Equal(b.FirstSeen) {
		return a.FirstSeen.After(b.FirstSeen)
	}
	return a.ContractID < b.ContractID
}

// ListUpgrades returns recorded interface upgrades, newest first.
func (m *Memory) ListUpgrades(ctx context.Context, f UpgradeFilter) (*UpgradePage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Normalize()

	m.mu.RLock()
	defer m.mu.RUnlock()

	var upgrades []Upgrade
	for k, specs := range m.specs {
		c, ok := m.contracts[k]
		if !ok || (f.Network != "" && c.Network != f.Network) {
			continue
		}
		// Save appends a spec only when a contract starts running a hash it
		// has not stored before, so this list is already in observation
		// order. The first entry is the registration, not an upgrade.
		for i := 1; i < len(specs); i++ {
			upgrades = append(upgrades, Upgrade{
				ContractID:  c.ContractID,
				Network:     c.Network,
				OldWasmHash: specs[i-1].WasmHash,
				NewWasmHash: specs[i].WasmHash,
				ObservedAt:  specs[i].DecodedAt,
			})
		}
	}

	sort.Slice(upgrades, func(i, j int) bool {
		if !upgrades[i].ObservedAt.Equal(upgrades[j].ObservedAt) {
			return upgrades[i].ObservedAt.After(upgrades[j].ObservedAt)
		}
		if upgrades[i].ContractID != upgrades[j].ContractID {
			return upgrades[i].ContractID < upgrades[j].ContractID
		}
		return upgrades[i].NewWasmHash < upgrades[j].NewWasmHash
	})

	page := &UpgradePage{Total: len(upgrades), Upgrades: []Upgrade{}}
	if f.Offset < len(upgrades) {
		end := min(f.Offset+f.Limit, len(upgrades))
		page.Upgrades = append(page.Upgrades, upgrades[f.Offset:end]...)
	}
	return page, nil
}

// ListContracts returns a filtered, paged listing ordered newest-refreshed
// first.
func (m *Memory) ListContracts(ctx context.Context, f ListFilter) (*ContractPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Normalize()

	m.mu.RLock()
	defer m.mu.RUnlock()

	matched := make([]Contract, 0, len(m.contracts))
	needle := strings.ToLower(f.Query)
	for k, c := range m.contracts {
		if f.Network != "" && c.Network != f.Network {
			continue
		}
		if needle != "" {
			c.Matches = matchSymbols(m.currentSymbols(k, c.CurrentWasmHash), f.Query)
			if len(c.Matches) == 0 &&
				!strings.Contains(strings.ToLower(c.ContractID), needle) &&
				!strings.Contains(strings.ToLower(c.Name), needle) {
				continue
			}
		}
		if len(f.Functions) > 0 || len(f.Types) > 0 {
			if !hasAll(m.currentInterface(k, c.CurrentWasmHash), f.Functions, f.Types) {
				continue
			}
		}
		c.MissingSince = copyMissing(c.MissingSince)
		matched = append(matched, c)
	}

	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].LastRefreshed.Equal(matched[j].LastRefreshed) {
			return matched[i].LastRefreshed.After(matched[j].LastRefreshed)
		}
		return matched[i].ContractID < matched[j].ContractID
	})

	page := &ContractPage{Total: len(matched), Contracts: []Contract{}}
	if f.Offset < len(matched) {
		end := min(f.Offset+f.Limit, len(matched))
		page.Contracts = append(page.Contracts, matched[f.Offset:end]...)
	}
	return page, nil
}

// ListContractsByWasm returns a paged listing of the contracts that run the
// given WASM hash, and — with Ever set — of the ones that ran it in the
// past.
func (m *Memory) ListContractsByWasm(ctx context.Context, f WasmFilter) (*WasmContractPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Normalize()

	m.mu.RLock()
	defer m.mu.RUnlock()

	matched := make([]WasmContract, 0, len(m.contracts))
	for k, c := range m.contracts {
		if f.Network != "" && c.Network != f.Network {
			continue
		}
		current := c.CurrentWasmHash == f.WasmHash
		if !current {
			if !f.Ever || !m.ranWasm(k, f.WasmHash) {
				continue
			}
		}
		c.MissingSince = copyMissing(c.MissingSince)
		matched = append(matched, WasmContract{Contract: c, Current: current})
	}

	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].LastRefreshed.Equal(matched[j].LastRefreshed) {
			return matched[i].LastRefreshed.After(matched[j].LastRefreshed)
		}
		return matched[i].ContractID < matched[j].ContractID
	})

	page := &WasmContractPage{Contracts: []WasmContract{}}
	if f.Offset < len(matched) {
		end := min(f.Offset+f.Limit, len(matched))
		page.Contracts = append(page.Contracts, matched[f.Offset:end]...)
	}
	page.Total = len(matched)
	return page, nil
}

// ranWasm reports whether a contract has a stored interface for the given
// WASM hash, which is how the store knows it ran that code at some point.
// The caller holds m.mu.
func (m *Memory) ranWasm(k, wasmHash string) bool {
	for _, s := range m.specs[k] {
		if s.WasmHash == wasmHash {
			return true
		}
	}
	return false
}

// currentSymbols returns the joined symbol names of the spec a contract
// currently runs. The caller holds m.mu.
func (m *Memory) currentSymbols(k, wasmHash string) string {
	for _, s := range m.specs[k] {
		if s.WasmHash == wasmHash {
			return joinSymbols(s.Interface)
		}
	}
	return ""
}

// currentInterface returns the decoded interface of the spec a contract
// currently runs. The caller holds m.mu.
func (m *Memory) currentInterface(k, wasmHash string) *model.Interface {
	for _, s := range m.specs[k] {
		if s.WasmHash == wasmHash {
			return s.Interface
		}
	}
	return nil
}

// GetSpec returns a stored interface, defaulting to the contract's current
// version when wasmHash is empty.
func (m *Memory) GetSpec(ctx context.Context, network, contractID, wasmHash string) (*Spec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	k := key(network, contractID)
	c, ok := m.contracts[k]
	if !ok {
		return nil, specNotFound(network, contractID, wasmHash)
	}

	want := wasmHash
	if want == "" {
		want = c.CurrentWasmHash
	}

	for i := range m.specs[k] {
		if m.specs[k][i].WasmHash == want {
			s := m.specs[k][i]
			cloned, err := cloneInterface(s.Interface)
			if err != nil {
				return nil, err
			}
			s.Interface = cloned
			return &s, nil
		}
	}
	return nil, specNotFound(network, contractID, wasmHash)
}

// ListVersions returns every stored version of a contract, newest first.
func (m *Memory) ListVersions(ctx context.Context, network, contractID string) ([]SpecVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	k := key(network, contractID)
	c, ok := m.contracts[k]
	if !ok || len(m.specs[k]) == 0 {
		return nil, fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}

	versions := make([]SpecVersion, 0, len(m.specs[k]))
	for _, s := range m.specs[k] {
		versions = append(versions, SpecVersion{
			WasmHash:  s.WasmHash,
			DecodedAt: s.DecodedAt,
			Current:   s.WasmHash == c.CurrentWasmHash,
		})
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i].DecodedAt.After(versions[j].DecodedAt)
	})
	return versions, nil
}

// cloneInterface deep-copies an interface via its JSON encoding, which is
// also how it is stored, so a caller can never mutate what the store holds.
func cloneInterface(iface *model.Interface) (*model.Interface, error) {
	if iface == nil {
		return nil, errors.New("store: cannot clone a nil interface")
	}
	body, err := json.Marshal(iface)
	if err != nil {
		return nil, fmt.Errorf("store: encoding interface: %w", err)
	}
	var out model.Interface
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("store: decoding interface: %w", err)
	}
	return &out, nil
}
