// Package api serves the registry over HTTP as JSON.
//
// The API is read-mostly: the only write is registering or refreshing a
// contract, which does not change the contract itself, only what SoroVault
// knows about it. There is no authentication — deployment is expected to put
// the service behind whatever the operator already uses.
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/soroworks/sorovault/internal/codegen"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
)

// Server holds the API's dependencies.
type Server struct {
	reg   *registry.Registry
	log   *slog.Logger
	cache *interfaceCache
}

// NewServer builds an API server over a registry.
func NewServer(reg *registry.Registry, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{reg: reg, log: log, cache: newInterfaceCache(interfaceCacheBytes)}
}

// Routes returns the API's routes, to be mounted under /api.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/contracts", s.listContracts)
	r.Post("/contracts", s.registerContract)
	r.Get("/wasm/{hash}", s.getWasmSpec)
	r.Post("/wasm/{hash}", s.catalogWasm)
	r.Get("/contracts/{id}", s.getContract)
	r.Put("/contracts/{id}/source", s.updateContractSource)
	r.Post("/contracts/{id}/refresh", s.refreshContract)
	r.Get("/contracts/{id}/versions", s.listVersions)
	r.Get("/contracts/{id}/functions/{fn}", s.getFunction)
	r.Get("/contracts/{id}/client.ts", s.getTypeScriptClient)
	r.Get("/openapi.json", serveOpenAPI)

	return r
}

var commitHash = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

func (s *Server) getWasmSpec(w http.ResponseWriter, r *http.Request) {
	wasmHash := strings.TrimSpace(chi.URLParam(r, "hash"))
	rawHash, err := hex.DecodeString(wasmHash)
	if err != nil || len(rawHash) != 32 {
		writeError(w, http.StatusBadRequest, "invalid wasm hash")
		return
	}
	wasmHash = hex.EncodeToString(rawHash)
	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stored, err := s.reg.Store().GetWasmSpec(r.Context(), network, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) catalogWasm(w http.ResponseWriter, r *http.Request) {
	result, err := s.reg.CatalogWasm(r.Context(), chi.URLParam(r, "hash"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (s *Server) updateContractSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sourceURL := strings.TrimSpace(req.SourceURL)
	parsed, err := url.Parse(sourceURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "source_url must be an absolute HTTP or HTTPS repository URL")
		return
	}
	commit := strings.TrimSpace(req.SourceCommit)
	if commit != "" && !commitHash.MatchString(commit) {
		writeError(w, http.StatusBadRequest, "source_commit must be a 40- or 64-character hexadecimal commit hash")
		return
	}
	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	contractID := chi.URLParam(r, "id")
	if err := s.reg.Store().UpdateSource(r.Context(), network, contractID, sourceURL, commit); err != nil {
		s.fail(w, r, err)
		return
	}
	contract, err := s.reg.Store().GetContract(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract)
}

// openAPISpec describes every route above. TestOpenAPICoversEveryRoute
// fails if a route is added without documenting it, or documented without
// existing.
//
//go:embed openapi.json
var openAPISpec []byte

func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(openAPISpec)
}

// getTypeScriptClient serves GET /api/contracts/{id}/client.ts: a typed
// TypeScript client for the contract's current interface, or for an older
// one with ?wasm_hash=. ?download=1 asks the browser to save it.
func (s *Server) getTypeScriptClient(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	source, err := codegen.TypeScript(stored.Interface, codegen.Source{
		ContractID: stored.ContractID,
		Network:    stored.Network,
		WasmHash:   stored.WasmHash,
	})
	if err != nil {
		// The interface decoded, but cannot be expressed as TypeScript —
		// a property of the contract, not a server fault.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+contractID+`.client.ts"`)
	}
	_, _ = w.Write([]byte(source))
}

// Middleware returns the stack the whole server runs behind: a request ID, a
// panic guard, and a bound on how long any one request may take.
func Middleware(log *slog.Logger) []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		middleware.RequestID,
		middleware.RealIP,
		middleware.Recoverer,
		requestLogger(log),
	}
}

// listContracts serves GET /api/contracts.
func (s *Server) listContracts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := store.ListFilter{
		Network: q.Get("network"),
		Query:   q.Get("q"),
		Limit:   intParam(q.Get("limit"), 0),
		Offset:  intParam(q.Get("offset"), 0),
	}
	filter.Normalize()

	page, err := s.reg.Store().ListContracts(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, listResponse{
		Contracts: page.Contracts,
		Total:     page.Total,
		Limit:     filter.Limit,
		Offset:    filter.Offset,
	})
}

// getContract serves GET /api/contracts/{id}, returning the decoded ABI.
//
// The optional wasm_hash parameter selects a superseded version; without it
// the contract's current interface is returned.
func (s *Server) getContract(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	contract, err := s.reg.Store().GetContract(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	cacheHash := wasmHash
	if cacheHash == "" {
		cacheHash = contract.CurrentWasmHash
	}
	key := specCacheKey{network: network, contractID: contractID, wasmHash: cacheHash}
	cached, ok := s.cache.get(key)
	if !ok {
		stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, cacheHash)
		if err != nil {
			s.fail(w, r, err)
			return
		}

		body, err := json.Marshal(stored.Interface)
		if err != nil {
			s.fail(w, r, fmt.Errorf("api: encoding interface for %s: %w", contractID, err))
			return
		}
		cached = cachedInterface{body: body, decodedAt: stored.DecodedAt.UTC()}
		s.cache.add(key, cached)
	}

	writeJSON(w, http.StatusOK, contractResponse{
		ContractID: contract.ContractID,
		Network:    contract.Network,
		WasmHash:   cacheHash,
		Name:       contract.Name,
		FirstSeen:  contract.FirstSeen.UTC(),
		DecodedAt:  cached.decodedAt,
		Current:    cacheHash == contract.CurrentWasmHash,
		Interface:  json.RawMessage(cached.body),
	})
}

// getFunction serves GET /api/contracts/{id}/functions/{fn}.
func (s *Server) getFunction(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")
	fnName := chi.URLParam(r, "fn")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	fn, ok := stored.Interface.Function(fnName)
	if !ok {
		writeError(w, http.StatusNotFound,
			"no function "+strconv.Quote(fnName)+" in contract "+contractID)
		return
	}

	writeJSON(w, http.StatusOK, functionResponse{
		ContractID: stored.ContractID,
		Network:    stored.Network,
		WasmHash:   stored.WasmHash,
		Signature:  fn.Signature(),
		Function:   fn,
	})
}

// listVersions serves GET /api/contracts/{id}/versions.
func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	versions, err := s.reg.Store().ListVersions(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, versionsResponse{
		ContractID: contractID,
		Network:    network,
		Versions:   versions,
	})
}

// registerContract serves POST /api/contracts.
func (s *Server) registerContract(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	contractID := strings.TrimSpace(req.ContractID)
	if contractID == "" {
		writeError(w, http.StatusBadRequest, "contract_id is required")
		return
	}

	result, err := s.reg.Register(r.Context(), contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

// refreshContract serves POST /api/contracts/{id}/refresh.
func (s *Server) refreshContract(w http.ResponseWriter, r *http.Request) {
	result, err := s.reg.Refresh(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// network resolves which network a read applies to: the explicit query
// parameter if given, otherwise the one this instance is pointed at.
func (s *Server) network(r *http.Request) (string, error) {
	if n := strings.TrimSpace(r.URL.Query().Get("network")); n != "" {
		return n, nil
	}
	return s.reg.Network(r.Context())
}

// fail maps a domain error onto a status code and writes it.
//
// Everything unrecognised becomes a 500 with a generic body, so an internal
// failure cannot leak a connection string or query text to a caller.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, stellar.ErrContractNotFound), errors.Is(err, stellar.ErrWasmNotFound):
		writeError(w, http.StatusNotFound, err.Error())

	case errors.Is(err, stellar.ErrNoWasm), errors.Is(err, spec.ErrNoSpecSection):
		// The request was well-formed and the contract exists; it just has
		// no interface to serve.
		writeError(w, http.StatusUnprocessableEntity, err.Error())

	case errors.Is(err, registry.ErrNetworkMismatch):
		writeError(w, http.StatusServiceUnavailable, err.Error())

	case isBadContractID(err), errors.Is(err, registry.ErrInvalidWasmHash):
		writeError(w, http.StatusBadRequest, err.Error())

	case errors.Is(err, r.Context().Err()) && r.Context().Err() != nil:
		// The client went away; there is nobody left to read a response.
		return

	default:
		s.log.ErrorContext(r.Context(), "request failed",
			"path", r.URL.Path, "method", r.Method, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// isBadContractID reports whether err came from strkey rejecting an address.
func isBadContractID(err error) bool {
	return strings.Contains(err.Error(), "is not a valid contract address") ||
		strings.Contains(err.Error(), "is not a contract address")
}

// intParam parses a query parameter, falling back on anything unparseable
// rather than rejecting the request over a malformed paging hint.
func intParam(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

// retryAfterSeconds renders a wait as the whole seconds a client should
// wait before retrying, which is what the Retry-After header asks for.
func retryAfterSeconds(d time.Duration) int {
	seconds := int((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}
