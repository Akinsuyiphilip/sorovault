package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/refresher"
	"github.com/soroworks/sorovault/internal/store"
)

// maxRequestBody caps how much a client may POST. Requests here carry a
// single contract ID, or a bounded list of them, so anything larger is a
// mistake or an attack.
const maxRequestBody = 64 << 10

// MaxBatchContracts is the most contract IDs one batch request may carry.
//
// Each registration costs two RPC round trips, so a batch of twenty is
// forty of them: enough to seed a deployment, few enough to finish inside
// the server's request timeout.
const MaxBatchContracts = 20

// The statuses a batch result can carry.
const (
	// batchStatusCreated means the contract was not in the registry before.
	batchStatusCreated = "created"
	// batchStatusUnchanged means the contract was already registered and
	// still runs the same interface.
	batchStatusUnchanged = "unchanged"
	// batchStatusChanged means the contract was already registered but has
	// been upgraded since it was last fetched.
	batchStatusChanged = "changed"
	// batchStatusError means the contract could not be registered.
	batchStatusError = "error"
)

type registerRequest struct {
	ContractID string `json:"contract_id"`
}

type batchRegisterRequest struct {
	ContractIDs []string `json:"contract_ids"`
}

// batchResult is the outcome of one contract in a batch.
type batchResult struct {
	ContractID string `json:"contract_id"`
	// Status is one of created, unchanged, changed or error.
	Status  string `json:"status"`
	Created bool   `json:"created"`
	Changed bool   `json:"changed"`
	// Contract and Interface are present when the registration worked.
	Contract  *store.Contract  `json:"contract,omitempty"`
	Interface *model.Interface `json:"interface,omitempty"`
	// Error and Code are present when it did not: Code is the status code
	// the contract would have had on its own.
	Error string `json:"error,omitempty"`
	Code  int    `json:"code,omitempty"`
}

// batchResponse is the outcome of a whole batch.
type batchResponse struct {
	Results []batchResult `json:"results"`
	// Created, Unchanged, Changed and Failed count the results by status.
	Created   int `json:"created"`
	Unchanged int `json:"unchanged"`
	Changed   int `json:"changed"`
	Failed    int `json:"failed"`
}

type sourceRequest struct {
	SourceURL    string `json:"source_url"`
	SourceCommit string `json:"source_commit,omitempty"`
}

type listResponse struct {
	Contracts []store.Contract `json:"contracts"`
	Total     int              `json:"total"`
	Limit     int              `json:"limit"`
	Offset    int              `json:"offset"`
}

type contractResponse struct {
	ContractID   string     `json:"contract_id"`
	Network      string     `json:"network"`
	WasmHash     string     `json:"wasm_hash"`
	Name         string     `json:"name,omitempty"`
	SourceURL    string     `json:"source_url,omitempty"`
	SourceCommit string     `json:"source_commit,omitempty"`
	FirstSeen    time.Time  `json:"first_seen"`
	DecodedAt    time.Time  `json:"decoded_at"`
	MissingSince *time.Time `json:"missing_since,omitempty"`

	// Current is false when an older version was requested explicitly.
	Current bool `json:"current"`

	Interface json.RawMessage `json:"interface"`
}

type functionResponse struct {
	ContractID string `json:"contract_id"`
	Network    string `json:"network"`
	WasmHash   string `json:"wasm_hash"`

	// Signature is the function rendered as a one-line Rust-like
	// declaration, for display and logging.
	Signature string `json:"signature"`

	Function model.Function `json:"function"`
}

type functionListResponse struct {
	ContractID string            `json:"contract_id"`
	Network    string            `json:"network"`
	WasmHash   string            `json:"wasm_hash"`
	Functions  []functionSummary `json:"functions"`
}

type functionSummary struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
}

type versionsResponse struct {
	ContractID string              `json:"contract_id"`
	Network    string              `json:"network"`
	Versions   []store.SpecVersion `json:"versions"`
}

// wasmContractsResponse is the answer to "who else runs this code?".
type wasmContractsResponse struct {
	WasmHash string `json:"wasm_hash"`
	Network  string `json:"network"`
	// Ever says whether the listing covers the contracts that ran the hash
	// in the past as well as the ones running it now.
	Ever      bool                 `json:"ever"`
	Contracts []store.WasmContract `json:"contracts"`
	Total     int                  `json:"total"`
	Limit     int                  `json:"limit"`
	Offset    int                  `json:"offset"`
}

type statsResponse struct {
	Networks       []store.NetworkStats   `json:"networks"`
	Contracts      int                    `json:"contracts"`
	Versions       int                    `json:"versions"`
	LatestContract *store.Contract        `json:"latest_contract,omitempty"`
	LastRefresh    *refresher.SweepRecord `json:"last_refresh,omitempty"`
}

type upgradeListResponse struct {
	Upgrades []store.Upgrade `json:"upgrades"`
	Total    int             `json:"total"`
	Limit    int             `json:"limit"`
	Offset   int             `json:"offset"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON sends a value as an HTTP JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out, so the response cannot be
		// corrected; the connection will simply be cut short.
		slog.Default().Error("writing json response", "error", err)
	}
}

// writeError sends a JSON error body.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

// decodeJSON reads a size-limited request body, rejecting unknown fields so
// a typo in a client's payload surfaces as an error rather than a silently
// ignored setting.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is empty")
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}

	// A second value in the same body means the client sent something other
	// than the single object this endpoint accepts.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

// requestLogger logs one line per request at debug level, and promotes
// server errors to error level so failures are visible at default settings.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			defer func() {
				attrs := []any{
					"method", r.Method,
					"path", r.URL.Path,
					"status", ww.Status(),
					"bytes", ww.BytesWritten(),
					"duration", time.Since(start),
					"request_id", middleware.GetReqID(r.Context()),
				}
				if ww.Status() >= http.StatusInternalServerError {
					log.ErrorContext(r.Context(), "request", attrs...)
					return
				}
				log.DebugContext(r.Context(), "request", attrs...)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
