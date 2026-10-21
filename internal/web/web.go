// Package web serves a small server-rendered browse UI over the registry.
//
// The UI is deliberately thin: it renders HTML from the same store the JSON
// API reads, and uses htmx only to swap the results table as the user types.
// Every page is a plain GET that works without JavaScript, so the htmx layer
// is an enhancement rather than a requirement.
package web

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
	"github.com/soroworks/sorovault/internal/wasm"
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
	r.Use(securityHeaders)

	r.Get("/", s.index)
	r.Post("/contracts", s.register)
	r.Get("/contracts/{id}", s.contract)
	r.Get("/partials/contracts", s.contractRows)
	r.Handle("/static/*", http.FileServer(http.FS(assets)))

	return r
}

// securityHeaders hardens every UI response.
//
// The browse UI renders strings that come from untrusted contracts. Go's
// html/template already escapes them, but a Content-Security-Policy is the
// second line of defence: if an escape ever fails, no script or style from a
// contract's text can run. Everything the UI needs is vendored under /static,
// so no other origin is allowed and neither 'unsafe-inline' nor 'unsafe-eval'
// is needed — htmx is a plain /static script and its default indicator
// stylesheet is switched off in layout.html so it injects no inline style.
func securityHeaders(next http.Handler) http.Handler {
	const policy = "default-src 'self'; base-uri 'none'; object-src 'none'; " +
		"frame-ancestors 'none'; form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", policy)
		h.Set("X-Content-Type-Options", "nosniff")
		// Legacy companion to frame-ancestors, for browsers that predate CSP.
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// index renders the searchable contract list.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.indexPage(w, r, http.StatusOK, indexData{})
}

// indexPage renders the index at a given status. It takes the data a failed
// registration left behind, so the visitor sees why their contract was not
// added with the ID they typed still in the form.
func (s *Server) indexPage(w http.ResponseWriter, r *http.Request, status int, data indexData) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	network := strings.TrimSpace(r.URL.Query().Get("network"))
	pageNumber := requestedPage(r)

	page, err := s.list(r, query, network, pageNumber)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	networks, err := s.reg.Store().ListNetworks(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	data.Query = query
	data.Network = network
	data.Networks = networks
	data.Contracts = page.Contracts
	data.Total = page.Total
	data.Page = pageNumber
	data.PageCount = pageCount(page.Total)

	s.renderStatus(w, r, status, "index.html", data)
}

// maxRegisterForm caps the registration form's body. It carries a single
// contract ID, so anything larger is a mistake.
const maxRegisterForm = 4 << 10

// register accepts the index page's registration form.
//
// The form posts here rather than to POST /api/contracts, because a browser
// needs an explanation on the page and a JSON error body would be one it
// cannot render. The status codes still carry the same meaning as the API's,
// so "not found" and "no interface" are told apart.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.registerFailed(w, r, "", http.StatusForbidden,
			"This form must be submitted from SoroVault.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRegisterForm)
	if err := r.ParseForm(); err != nil {
		s.registerFailed(w, r, "", http.StatusBadRequest, "The form could not be read.")
		return
	}

	contractID := strings.TrimSpace(r.PostForm.Get("contract_id"))
	if contractID == "" {
		s.registerFailed(w, r, contractID, http.StatusBadRequest,
			"Enter a contract ID to register.")
		return
	}

	result, err := s.reg.Register(r.Context(), contractID)
	if err != nil {
		status, message := registerStatus(err)
		if status == http.StatusInternalServerError {
			s.log.ErrorContext(r.Context(), "registering from the UI",
				"contract_id", contractID, "error", err)
		}
		if errors.Is(err, registry.ErrBusy) {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter(s.reg.AcquireTimeout())))
		}
		s.registerFailed(w, r, contractID, status, message)
		return
	}

	target := "/contracts/" + url.PathEscape(contractID) +
		"?network=" + url.QueryEscape(result.Contract.Network)

	if isHtmx(r) {
		// htmx cannot follow a 303 usefully; it follows this header instead.
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// registerFailed shows a registration error in place: the form alone when
// htmx asked for it, and the whole page otherwise, so the form works with
// scripting off.
func (s *Server) registerFailed(w http.ResponseWriter, r *http.Request, contractID string, status int, message string) {
	if isHtmx(r) {
		// htmx only swaps a successful response, so the fragment goes out
		// under 200 and keeps the status for the log, not the browser.
		s.renderStatus(w, r, http.StatusOK, "register.html", registerData{
			ContractID: contractID,
			Error:      message,
		})
		return
	}

	s.indexPage(w, r, status, indexData{ContractID: contractID, RegisterError: message})
}

// sameOrigin reports whether a form write provably came from this origin.
//
// The UI has no session and sets no cookie, so a CSRF token would have
// nothing to bind to. What a cross-site POST could still do is make a
// visitor's browser register a contract they did not ask for, so the write
// is accepted only when the browser names this origin: Origin is what a
// browser sends for a cross-site POST and a page cannot forge it, with
// Referer as the fallback older clients use. Neither is present for a
// programmatic client, which is what the JSON API is for.
func sameOrigin(r *http.Request) bool {
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return false
	}

	parsed, err := url.Parse(source)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func isHtmx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// registerStatus maps a registration error onto the status and wording the
// form shows. It mirrors internal/api's mapping, so an error means the same
// thing whichever door the request came through.
func registerStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, stellar.ErrContractNotFound), errors.Is(err, stellar.ErrWasmNotFound):
		return http.StatusNotFound, "No contract with that ID was found on this network."

	case errors.Is(err, stellar.ErrNoWasm), errors.Is(err, spec.ErrNoSpecSection):
		return http.StatusUnprocessableEntity, "The contract exists but has no interface to decode."

	case errors.Is(err, spec.ErrLimitExceeded), errors.Is(err, wasm.ErrModuleTooLarge):
		return http.StatusUnprocessableEntity, "The contract's interface is too large to decode."

	case errors.Is(err, registry.ErrNetworkMismatch):
		return http.StatusServiceUnavailable, "This registry is not serving the network its RPC endpoint reports."

	case errors.Is(err, registry.ErrBusy):
		return http.StatusServiceUnavailable, "The registry is busy; try again in a moment."

	case isBadContractID(err):
		return http.StatusBadRequest, "That is not a valid contract ID."

	default:
		return http.StatusInternalServerError, "Something went wrong registering that contract."
	}
}

// isBadContractID reports whether err came from strkey rejecting an address.
func isBadContractID(err error) bool {
	return strings.Contains(err.Error(), "is not a valid contract address") ||
		strings.Contains(err.Error(), "is not a contract address")
}

// retryAfter renders a wait as the whole seconds a Retry-After header asks
// for.
func retryAfter(d time.Duration) int {
	seconds := int((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
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

	// Gather all networks this contract is registered on.
	allNetworks, err := s.reg.Store().ListNetworks(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	contractNetworks := make([]string, 0, len(allNetworks))
	for _, net := range allNetworks {
		_, err2 := s.reg.Store().GetContract(r.Context(), net, contractID)
		if err2 == nil {
			contractNetworks = append(contractNetworks, net)
		}
	}

	s.render(w, r, "contract.html", contractData{
		Contract:  *contract,
		Spec:      *stored,
		Versions:  versions,
		Interface: stored.Interface,
		IsCurrent: stored.WasmHash == contract.CurrentWasmHash,
		Networks:  contractNetworks,
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
	Networks  []string
	Contracts []store.Contract
	Total     int
	Page      int
	PageCount int

	// ContractID and RegisterError are what a failed registration leaves
	// behind: the ID to correct, and why it was refused.
	ContractID    string
	RegisterError string
}

// registerData is the registration form and the message under it.
type registerData struct {
	ContractID string
	Error      string
}

type contractData struct {
	Contract  store.Contract
	Spec      store.Spec
	Versions  []store.SpecVersion
	Interface *model.Interface
	IsCurrent bool
	Networks  []string
}

// render writes a template, buffering it first so that a failure midway
// through cannot emit a half-written page under a 200 status.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	s.renderStatus(w, r, http.StatusOK, name, data)
}

// renderStatus is render at a given status, used when a page carries an
// error the browser should also see in the status line.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var buf strings.Builder
	if err := s.templates.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.ErrorContext(r.Context(), "rendering template", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
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

type renderPart struct {
	Text  string
	Class string
	Href  string
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// funcs are the helpers templates use to render the ABI model.
func funcs() template.FuncMap {
	return template.FuncMap{
		"renderType":     model.Render,
		"signatureParts": signatureParts,
		"typeParts":      typeParts,
		"typeID":         typeID,
		"markdown":       renderMarkdown,
		"anchor":         anchor,
		"anchorID":       anchorID,
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
		"registerForm": func(contractID, message string) registerData {
			return registerData{ContractID: contractID, Error: message}
		},
	}
}

// anchorLink is one symbol's permalink on the contract page. ID is what the
// heading's id and the link's href both use; Label is what a screen reader
// announces.
type anchorLink struct {
	ID    string
	Label string
}

// symbolKinds maps an anchor prefix to the word its label uses. The prefix
// keeps a function and a type of the same name apart.
var symbolKinds = map[string]string{
	"fn":    "function",
	"type":  "type",
	"event": "event",
}

// anchorID builds a symbol's anchor id. html/template escapes the value
// wherever it is written, so a contract cannot break the page out of an id
// or a fragment with a name of its own.
func anchorID(kind, name string) string { return kind + "-" + name }

func anchor(kind, name string) anchorLink {
	label := kind + " " + name
	if word, ok := symbolKinds[kind]; ok {
		label = word + " " + name
	}
	return anchorLink{ID: anchorID(kind, name), Label: label}
}

func typeID(name string) string { return anchorID("type", name) }

func signatureParts(fn model.Function, iface *model.Interface) []renderPart {
	parts := []renderPart{{Text: "fn ", Class: "sig-keyword"}, {Text: fn.Name, Class: "sig-function"}, {Text: "(", Class: "sig-punctuation"}}
	for i, input := range fn.Inputs {
		if i > 0 {
			parts = append(parts, renderPart{Text: ", ", Class: "sig-punctuation"})
		}
		parts = append(parts,
			renderPart{Text: input.Name, Class: "sig-parameter"},
			renderPart{Text: ": ", Class: "sig-punctuation"},
		)
		parts = append(parts, typeParts(input.Type, iface)...)
	}
	parts = append(parts, renderPart{Text: ")", Class: "sig-punctuation"})
	if len(fn.Outputs) > 0 {
		parts = append(parts, renderPart{Text: " -> ", Class: "sig-punctuation"})
		parts = append(parts, typeParts(fn.Outputs[0], iface)...)
	}
	return parts
}

func typeParts(value model.Type, iface *model.Interface) []renderPart {
	part := func(text, class string) []renderPart {
		return []renderPart{{Text: text, Class: class}}
	}
	child := func(child *model.Type) []renderPart {
		if child == nil {
			return part("?", "type-name")
		}
		return typeParts(*child, iface)
	}
	wrap := func(name string, inner ...[]renderPart) []renderPart {
		parts := []renderPart{{Text: name, Class: "type-constructor"}, {Text: "<", Class: "type-punctuation"}}
		for i, group := range inner {
			if i > 0 {
				parts = append(parts, renderPart{Text: ", ", Class: "type-punctuation"})
			}
			parts = append(parts, group...)
		}
		return append(parts, renderPart{Text: ">", Class: "type-punctuation"})
	}

	switch value.Kind {
	case model.KindOption:
		return wrap("Option", child(value.Inner))
	case model.KindVec:
		return wrap("Vec", child(value.Element))
	case model.KindMap:
		return wrap("Map", child(value.Key), child(value.Value))
	case model.KindResult:
		return wrap("Result", child(value.Ok), child(value.Error))
	case model.KindBytesN:
		return wrap("BytesN", part(strconv.FormatUint(uint64(value.N), 10), "type-parameter"))
	case model.KindTuple:
		parts := []renderPart{{Text: "(", Class: "type-punctuation"}}
		for i, element := range value.Elements {
			if i > 0 {
				parts = append(parts, renderPart{Text: ", ", Class: "type-punctuation"})
			}
			parts = append(parts, typeParts(element, iface)...)
		}
		return append(parts, renderPart{Text: ")", Class: "type-punctuation"})
	case model.KindUDT:
		name := value.Name
		for _, definition := range definedTypes(iface) {
			if definition == name {
				return []renderPart{{Text: name, Class: "type-reference", Href: "#" + typeID(name)}}
			}
		}
		return part(name, "type-reference")
	default:
		return part(model.Render(value), "type-name")
	}
}

func definedTypes(iface *model.Interface) []string {
	if iface == nil {
		return nil
	}
	names := make([]string, 0, len(iface.Types.Structs)+len(iface.Types.Unions)+len(iface.Types.Enums)+len(iface.Types.ErrorEnums))
	for _, value := range iface.Types.Structs {
		names = append(names, value.Name)
	}
	for _, value := range iface.Types.Unions {
		names = append(names, value.Name)
	}
	for _, value := range iface.Types.Enums {
		names = append(names, value.Name)
	}
	for _, value := range iface.Types.ErrorEnums {
		names = append(names, value.Name)
	}
	return names
}
