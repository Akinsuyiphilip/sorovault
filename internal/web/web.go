// Package web serves a small server-rendered browse UI over the registry.
//
// The UI is deliberately thin: it renders HTML from the same store the JSON
// API reads, and uses htmx only to swap the results table as the user types.
// Every page is a plain GET that works without JavaScript, so the htmx layer
// is an enhancement rather than a requirement.
package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server renders the browse UI.
type Server struct {
	reg       *registry.Registry
	log       *slog.Logger
	templates *template.Template
}

const contractsPerPage = 100

// NewServer builds the UI server, parsing its templates up front so a
// template error is a startup failure rather than a 500 in production.
func NewServer(reg *registry.Registry, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	tmpl, err := template.New("").Funcs(funcs()).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}

	return &Server{reg: reg, log: log, templates: tmpl}, nil
}

// Routes returns the UI's routes, to be mounted at the server root.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/", s.index)
	r.Get("/contracts/{id}", s.contract)
	r.Get("/partials/contracts", s.contractRows)
	r.Handle("/static/*", http.FileServer(http.FS(assets)))

	return r
}

// index renders the searchable contract list.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	network := strings.TrimSpace(r.URL.Query().Get("network"))
	pageNumber := requestedPage(r)

	page, err := s.list(r, query, network, pageNumber)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.render(w, r, "index.html", indexData{
		Query:     query,
		Network:   network,
		Contracts: page.Contracts,
		Total:     page.Total,
		Page:      pageNumber,
		PageCount: pageCount(page.Total),
	})
}

// contractRows renders just the results table, which htmx swaps in as the
// search box changes.
func (s *Server) contractRows(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	network := strings.TrimSpace(r.URL.Query().Get("network"))
	pageNumber := requestedPage(r)

	page, err := s.list(r, query, network, pageNumber)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.render(w, r, "contract_rows.html", indexData{
		Query:     query,
		Network:   network,
		Contracts: page.Contracts,
		Total:     page.Total,
		Page:      pageNumber,
		PageCount: pageCount(page.Total),
	})
}

// contract renders one contract's decoded interface.
func (s *Server) contract(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network := strings.TrimSpace(r.URL.Query().Get("network"))
	if network == "" {
		var err error
		if network, err = s.reg.Network(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
	}

	contract, err := s.reg.Store().GetContract(r.Context(), network, contractID)
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

	versions, err := s.reg.Store().ListVersions(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.render(w, r, "contract.html", contractData{
		Contract:  *contract,
		Spec:      *stored,
		Versions:  versions,
		Interface: stored.Interface,
		IsCurrent: stored.WasmHash == contract.CurrentWasmHash,
	})
}

// list runs a listing query for the UI, capped to a single screenful.
func (s *Server) list(r *http.Request, query, network string, page int) (*store.ContractPage, error) {
	filter := store.ListFilter{
		Query:   query,
		Network: network,
		Limit:   contractsPerPage,
		Offset:  pageOffset(page),
	}
	filter.Normalize()
	return s.reg.Store().ListContracts(r.Context(), filter)
}

func requestedPage(r *http.Request) int {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func pageOffset(page int) int {
	if page <= 1 {
		return 0
	}

	maxInt := int(^uint(0) >> 1)
	if page-1 > maxInt/contractsPerPage {
		return maxInt
	}
	return (page - 1) * contractsPerPage
}

func pageCount(total int) int {
	if total <= 0 {
		return 0
	}
	return (total + contractsPerPage - 1) / contractsPerPage
}

type indexData struct {
	Query     string
	Network   string
	Contracts []store.Contract
	Total     int
	Page      int
	PageCount int
}

type contractData struct {
	Contract  store.Contract
	Spec      store.Spec
	Versions  []store.SpecVersion
	Interface *model.Interface
	IsCurrent bool
}

// render writes a template, buffering it first so that a failure midway
// through cannot emit a half-written page under a 200 status.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf strings.Builder
	if err := s.templates.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.ErrorContext(r.Context(), "rendering template", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(buf.String()))
}

// fail renders an error page, mapping a missing record to a 404.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	message := "Something went wrong."

	if isNotFound(err) {
		status = http.StatusNotFound
		message = "No such contract in this registry."
	} else {
		s.log.ErrorContext(r.Context(), "ui request failed", "path", r.URL.Path, "error", err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if err := s.templates.ExecuteTemplate(w, "error.html", errorData{
		Status:  status,
		Message: message,
	}); err != nil {
		s.log.ErrorContext(r.Context(), "rendering error page", "error", err)
	}
}

type errorData struct {
	Status  int
	Message string
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// funcs are the helpers templates use to render the ABI model.
func funcs() template.FuncMap {
	return template.FuncMap{
		"renderType": model.Render,
		"signature":  func(f model.Function) string { return f.Signature() },
		"pageURL": func(query, network string, page int) string {
			values := url.Values{}
			if query != "" {
				values.Set("q", query)
			}
			if network != "" {
				values.Set("network", network)
			}
			if page > 1 {
				values.Set("page", strconv.Itoa(page))
			}
			if encoded := values.Encode(); encoded != "" {
				return "/?" + encoded
			}
			return "/"
		},
		"dec": func(n int) int { return n - 1 },
		"inc": func(n int) int { return n + 1 },
		"short": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
		"hasTypes": func(t model.Types) bool {
			return len(t.Structs)+len(t.Unions)+len(t.Enums)+len(t.ErrorEnums) > 0
		},
	}
}
