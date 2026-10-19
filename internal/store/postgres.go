package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/soroworks/sorovault/internal/model"
)

// Postgres is the Store backed by a Postgres database.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// NewPostgres connects to databaseURL and verifies the connection.
func NewPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: pinging postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// NewPostgresFromPool wraps an existing pool. Tests use it to share a pool
// across cases; Close then leaves the pool open for the caller to manage.
func NewPostgresFromPool(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// Ping checks that the database at databaseURL answers.
//
// It opens nothing but a connection and changes nothing, so a health check
// can use it without leaving a trace.
func Ping(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("store: connecting to postgres: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: pinging postgres: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

// SaveWasmSpec stores an interface for uploaded code independent of contracts.
func (p *Postgres) SaveWasmSpec(ctx context.Context, network, wasmHash string, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}
	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("store: encoding interface: %w", err)
	}
	const q = `
		INSERT INTO wasm_specs (network, wasm_hash, spec)
		VALUES ($1, $2, $3)
		ON CONFLICT (network, wasm_hash) DO UPDATE SET spec = EXCLUDED.spec, decoded_at = now()`
	if _, err := p.pool.Exec(ctx, q, network, wasmHash, body); err != nil {
		return fmt.Errorf("store: saving wasm spec %s: %w", wasmHash, err)
	}
	return nil
}

// GetWasmSpec returns a hash-addressed interface without requiring a contract.
func (p *Postgres) GetWasmSpec(ctx context.Context, network, wasmHash string) (*WasmSpec, error) {
	const q = `SELECT network, wasm_hash, spec, decoded_at FROM wasm_specs WHERE network = $1 AND wasm_hash = $2`
	var s WasmSpec
	var body []byte
	err := p.pool.QueryRow(ctx, q, network, wasmHash).Scan(&s.Network, &s.WasmHash, &body, &s.DecodedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: wasm spec %s on %s", ErrNotFound, wasmHash, network)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading wasm spec %s: %w", wasmHash, err)
	}
	var iface model.Interface
	if err := json.Unmarshal(body, &iface); err != nil {
		return nil, fmt.Errorf("store: decoding wasm spec %s: %w", wasmHash, err)
	}
	s.Interface = &iface
	return &s, nil
}

// Save upserts the contract and appends its spec version, in one transaction
// so a contract row can never end up pointing at a spec that was not stored.
func (p *Postgres) Save(ctx context.Context, c Contract, iface *model.Interface) error {
	if iface == nil {
		return errors.New("store: cannot save a nil interface")
	}

	body, err := json.Marshal(iface)
	if err != nil {
		return fmt.Errorf("store: encoding interface: %w", err)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once the tx is committed

	// first_seen is preserved across refreshes; everything else tracks the
	// most recent successful fetch.
	const upsertContract = `
		INSERT INTO contracts (network, contract_id, current_wasm_hash, name, first_seen, last_refreshed)
		VALUES ($1, $2, $3, $4, now(), now())
		ON CONFLICT (network, contract_id) DO UPDATE SET
			current_wasm_hash = EXCLUDED.current_wasm_hash,
			name              = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE contracts.name END,
			last_refreshed    = now()`

	if _, err := tx.Exec(ctx, upsertContract, c.Network, c.ContractID, c.CurrentWasmHash, c.Name); err != nil {
		return fmt.Errorf("store: saving contract %s: %w", c.ContractID, err)
	}

	// Re-registering an unchanged contract must not append a duplicate
	// version, so a repeat hash refreshes the existing row in place.
	const insertSpec = `
		INSERT INTO specs (network, contract_id, wasm_hash, spec, symbols)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (network, contract_id, wasm_hash) DO UPDATE SET
			spec    = EXCLUDED.spec,
			symbols = EXCLUDED.symbols`

	if _, err := tx.Exec(ctx, insertSpec,
		c.Network, c.ContractID, c.CurrentWasmHash, body, joinSymbols(iface)); err != nil {
		return fmt.Errorf("store: saving spec for %s: %w", c.ContractID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// GetContract returns one contract's current state.
func (p *Postgres) GetContract(ctx context.Context, network, contractID string) (*Contract, error) {
	const q = `
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit, first_seen, last_refreshed
		FROM contracts
		WHERE network = $1 AND contract_id = $2`

	var c Contract
	err := p.pool.QueryRow(ctx, q, network, contractID).Scan(
		&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit, &c.FirstSeen, &c.LastRefreshed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading contract %s: %w", contractID, err)
	}
	return &c, nil
}

// UpdateSource attaches repository metadata to an existing contract.
func (p *Postgres) UpdateSource(ctx context.Context, network, contractID, sourceURL, sourceCommit string) error {
	const q = `UPDATE contracts SET source_url = $3, source_commit = $4 WHERE network = $1 AND contract_id = $2`
	tag, err := p.pool.Exec(ctx, q, network, contractID, sourceURL, sourceCommit)
	if err != nil {
		return fmt.Errorf("store: updating source for %s: %w", contractID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return nil
}

// ListContracts returns a filtered, paged listing ordered by most recently
// refreshed, along with the total number of matches.
func (p *Postgres) ListContracts(ctx context.Context, f ListFilter) (*ContractPage, error) {
	f.Normalize()

	// $1 empty means "any network", $2 empty means "any text" — expressing
	// both as SQL keeps this a single prepared statement rather than
	// concatenated fragments.
	// Symbols come from the spec the contract currently runs, so a function
	// removed by an upgrade stops matching.
	const q = `
		WITH matched AS (
			SELECT c.network, c.contract_id, c.current_wasm_hash, c.name, c.source_url, c.source_commit,
			       c.first_seen, c.last_refreshed, COALESCE(s.symbols, '') AS symbols
			FROM contracts c
			LEFT JOIN specs s
			  ON s.network = c.network AND s.contract_id = c.contract_id
			 AND s.wasm_hash = c.current_wasm_hash
			WHERE ($1 = '' OR c.network = $1)
			  AND ($2 = '' OR c.contract_id ILIKE '%' || $2 || '%'
			               OR c.name ILIKE '%' || $2 || '%'
			               OR s.symbols ILIKE '%' || $2 || '%')
		)
		SELECT network, contract_id, current_wasm_hash, name, source_url, source_commit, first_seen, last_refreshed,
		       symbols, count(*) OVER () AS total
		FROM matched
		ORDER BY last_refreshed DESC, contract_id
		LIMIT $3 OFFSET $4`

	rows, err := p.pool.Query(ctx, q, f.Network, escapeLike(f.Query), f.Limit, f.Offset)
	if err != nil {
		return nil, fmt.Errorf("store: listing contracts: %w", err)
	}
	defer rows.Close()

	page := &ContractPage{Contracts: []Contract{}}
	for rows.Next() {
		var c Contract
		var symbols string
		var total int
		if err := rows.Scan(
			&c.Network, &c.ContractID, &c.CurrentWasmHash, &c.Name, &c.SourceURL, &c.SourceCommit,
			&c.FirstSeen, &c.LastRefreshed, &symbols, &total,
		); err != nil {
			return nil, fmt.Errorf("store: scanning contract: %w", err)
		}
		c.Matches = matchSymbols(symbols, f.Query)
		page.Contracts = append(page.Contracts, c)
		page.Total = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing contracts: %w", err)
	}

	// The window function only reports a total on rows that came back, so a
	// page past the end needs a separate count to stay accurate.
	if len(page.Contracts) == 0 && f.Offset > 0 {
		total, err := p.countContracts(ctx, f)
		if err != nil {
			return nil, err
		}
		page.Total = total
	}

	return page, nil
}

// ListNetworks returns the distinct networks represented in the registry.
func (p *Postgres) ListNetworks(ctx context.Context) ([]string, error) {
	const q = `SELECT DISTINCT network FROM contracts ORDER BY network`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: listing networks: %w", err)
	}
	defer rows.Close()

	networks := []string{}
	for rows.Next() {
		var network string
		if err := rows.Scan(&network); err != nil {
			return nil, fmt.Errorf("store: scanning network: %w", err)
		}
		networks = append(networks, network)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing networks: %w", err)
	}
	return networks, nil
}

func (p *Postgres) countContracts(ctx context.Context, f ListFilter) (int, error) {
	// Must mirror ListContracts' predicate, or a page past the end would
	// report a different total from the pages before it.
	const q = `
		SELECT count(*)
		FROM contracts c
		LEFT JOIN specs s
		  ON s.network = c.network AND s.contract_id = c.contract_id
		 AND s.wasm_hash = c.current_wasm_hash
		WHERE ($1 = '' OR c.network = $1)
		  AND ($2 = '' OR c.contract_id ILIKE '%' || $2 || '%'
		               OR c.name ILIKE '%' || $2 || '%'
		               OR s.symbols ILIKE '%' || $2 || '%')`

	var total int
	if err := p.pool.QueryRow(ctx, q, f.Network, escapeLike(f.Query)).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: counting contracts: %w", err)
	}
	return total, nil
}

// GetSpec returns a stored interface. An empty wasmHash selects the version
// the contract currently runs.
func (p *Postgres) GetSpec(ctx context.Context, network, contractID, wasmHash string) (*Spec, error) {
	// Joining against contracts for the empty-hash case keeps "current"
	// defined by the contract row rather than by insertion order, which
	// matters if a contract is ever rolled back to an earlier WASM.
	const q = `
		SELECT s.id, s.network, s.contract_id, s.wasm_hash, s.spec, s.decoded_at
		FROM specs s
		JOIN contracts c ON c.network = s.network AND c.contract_id = s.contract_id
		WHERE s.network = $1
		  AND s.contract_id = $2
		  AND s.wasm_hash = CASE WHEN $3 = '' THEN c.current_wasm_hash ELSE $3 END`

	var s Spec
	var body []byte
	err := p.pool.QueryRow(ctx, q, network, contractID, wasmHash).Scan(
		&s.ID, &s.Network, &s.ContractID, &s.WasmHash, &body, &s.DecodedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, specNotFound(network, contractID, wasmHash)
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading spec for %s: %w", contractID, err)
	}

	var iface model.Interface
	if err := json.Unmarshal(body, &iface); err != nil {
		return nil, fmt.Errorf("store: decoding stored spec for %s: %w", contractID, err)
	}
	s.Interface = &iface

	return &s, nil
}

// ListVersions returns every stored version of a contract, newest first,
// flagging whichever one the contract currently runs.
func (p *Postgres) ListVersions(ctx context.Context, network, contractID string) ([]SpecVersion, error) {
	const q = `
		SELECT s.wasm_hash, s.decoded_at, s.wasm_hash = c.current_wasm_hash AS current
		FROM specs s
		JOIN contracts c ON c.network = s.network AND c.contract_id = s.contract_id
		WHERE s.network = $1 AND s.contract_id = $2
		ORDER BY s.decoded_at DESC, s.id DESC`

	rows, err := p.pool.Query(ctx, q, network, contractID)
	if err != nil {
		return nil, fmt.Errorf("store: listing versions for %s: %w", contractID, err)
	}
	defer rows.Close()

	versions := []SpecVersion{}
	for rows.Next() {
		var v SpecVersion
		if err := rows.Scan(&v.WasmHash, &v.DecodedAt, &v.Current); err != nil {
			return nil, fmt.Errorf("store: scanning version: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing versions for %s: %w", contractID, err)
	}

	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: contract %s on %s", ErrNotFound, contractID, network)
	}
	return versions, nil
}

func specNotFound(network, contractID, wasmHash string) error {
	if wasmHash == "" {
		return fmt.Errorf("%w: spec for %s on %s", ErrNotFound, contractID, network)
	}
	return fmt.Errorf("%w: spec %s for %s on %s", ErrNotFound, wasmHash, contractID, network)
}

// escapeLike neutralises the wildcards in a user-supplied search term so that
// a query containing "%" or "_" matches those characters literally instead of
// silently matching far more than it appears to. Backslash is LIKE's default
// escape character, so it has to be doubled first.
//
// This is applied where the term meets SQL rather than in ListFilter, so that
// stores which match in Go still see the term the user typed.
func escapeLike(q string) string {
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, "%", `\%`)
	q = strings.ReplaceAll(q, "_", `\_`)
	return q
}
