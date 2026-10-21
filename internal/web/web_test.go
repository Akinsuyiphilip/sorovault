package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
	"github.com/soroworks/sorovault/internal/web"
)

const (
	idA = "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN"
	idB = "CDZZZPYQALFRBQAZD5KCW2SZK4ZF455PJVD7H53AGHXTWAJO4BYVPP2V"
)

type harness struct {
	server *httptest.Server
	fake   *stellartest.Fake
	reg    *registry.Registry
	store  *store.Memory
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	fake := stellartest.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	memory := store.NewMemory()
	memory.SetClock(func() time.Time { return time.Unix(0, 0) })
	reg := registry.New(fake, spec.XDRDecoder{}, memory, registry.Options{Logger: log})

	ui, err := web.NewServer(reg, log)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Mount("/", ui.Routes())

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &harness{server: srv, fake: fake, reg: reg, store: memory}
}

func (h *harness) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.server.URL+path, nil)
	require.NoError(t, err)

	resp, err := h.server.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(raw)
}

// postForm submits the registration form without following the redirect, so
// a test can assert on the status and Location it produced.
func (h *harness) postForm(t *testing.T, path string, form url.Values, headers map[string]string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		h.server.URL+path, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(raw)
}

// deploy puts the real testnet fixture on the fake network under the given
// ID, without registering it.
func (h *harness) deploy(t *testing.T, id string) {
	t.Helper()

	module, err := os.ReadFile("../spec/testdata/zkvote.wasm")
	require.NoError(t, err)
	h.fake.Deploy(id, module)
}

// register puts the real testnet fixture into the registry under the given ID.
func (h *harness) register(t *testing.T, id string) {
	t.Helper()

	module, err := os.ReadFile("../spec/testdata/zkvote.wasm")
	require.NoError(t, err)

	h.fake.Deploy(id, module)
	_, err = h.reg.Register(context.Background(), id)
	require.NoError(t, err)
}

// TestTemplatesParse is the guard that matters most for a template-driven
// UI: NewServer parses everything up front, so a broken template is a
// startup failure rather than a 500 in production.
func TestTemplatesParse(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := registry.New(stellartest.New(), spec.XDRDecoder{}, store.NewMemory(), registry.Options{Logger: log})

	_, err := web.NewServer(reg, log)
	require.NoError(t, err)
}

func TestIndexEmpty(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	resp, body := h.get(t, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, body, "No contracts registered yet")
	assert.Contains(t, body, "sorovault add")
}

// TestIndexListsContracts proves a contract that has gone missing is
// marked in the listing, so a reader can tell it apart from a live one.
func TestIndexMarksMissingContracts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	h.fake.Remove(idA)
	_, err := h.reg.Refresh(context.Background(), idA)
	require.NoError(t, err)

	resp, body := h.get(t, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "missing",
		"the listing marks a contract that is no longer on the network")

	resp, body = h.get(t, "/contracts/"+idA)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "not on the network any more")
}

func TestIndexListsContracts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	resp, body := h.get(t, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, idA)
	assert.Contains(t, body, "1 contract")
	assert.Contains(t, body, `id="contract-rows"`)
}

func TestIndexNetworkSelectorFiltersResults(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	iface := &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions:  []model.Function{},
		Types: model.Types{
			Structs: []model.Struct{}, Unions: []model.Union{},
			Enums: []model.Enum{}, ErrorEnums: []model.ErrorEnum{},
		},
		Events: []model.Event{},
	}
	for _, c := range []store.Contract{
		{ContractID: idA, Network: "testnet", CurrentWasmHash: "hash-testnet"},
		{ContractID: idB, Network: "public", CurrentWasmHash: "hash-public"},
	} {
		require.NoError(t, h.reg.Store().Save(context.Background(), c, iface))
	}

	_, body := h.get(t, "/")
	assert.Contains(t, body, `<option value="public">public</option>`)
	assert.Contains(t, body, `<option value="testnet">testnet</option>`)
	assert.Contains(t, body, `<option value="" selected>All</option>`)

	_, body = h.get(t, "/?network=public")
	assert.Contains(t, body, idB)
	assert.NotContains(t, body, idA)
	assert.Contains(t, body, `<option value="public" selected>public</option>`)

	_, body = h.get(t, "/partials/contracts?network=public")
	assert.Contains(t, body, idB)
	assert.NotContains(t, body, idA)
}

func TestIndexPagination(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	for i := 0; i < 101; i++ {
		err := h.reg.Store().Save(context.Background(), store.Contract{
			ContractID:      fmt.Sprintf("contract-%03d", i),
			Network:         "testnet",
			CurrentWasmHash: fmt.Sprintf("hash-%03d", i),
		}, &model.Interface{ABIVersion: model.ABIVersion})
		require.NoError(t, err)
	}

	for _, path := range []string{
		"/?page=2&q=contract&network=testnet",
		"/partials/contracts?page=2&q=contract&network=testnet",
	} {
		resp, body := h.get(t, path)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "Page 2 of 2")
		assert.Contains(t, body, "contract-100")
		assert.NotContains(t, body, "contract-099")
		// Page 1 is the canonical URL, so its link carries no page parameter;
		// the template HTML-escapes the query string's ampersands.
		assert.Contains(t, body, `href="/?network=testnet&amp;q=contract">Previous</a>`)
		assert.NotContains(t, body, `page=3`, "there is no third page to link to")
	}
}

func TestSearch(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)
	h.register(t, idB)

	t.Run("matching", func(t *testing.T) {
		_, body := h.get(t, "/?q=CDZZXV")
		assert.Contains(t, body, idA)
		assert.NotContains(t, body, idB)
	})

	t.Run("no match", func(t *testing.T) {
		_, body := h.get(t, "/?q=nothing-matches")
		assert.Contains(t, body, "No contract matches")
	})

	t.Run("by function name shows what matched", func(t *testing.T) {
		// The zkvote fixture declares has_voted and vote.
		_, body := h.get(t, "/?q=voted")
		assert.Contains(t, body, idA)
		assert.Contains(t, body, idB)
		assert.Contains(t, body, "<code>has_voted</code>")
	})
}

// TestPartialIsAFragment checks the htmx endpoint returns just the swappable
// region, not a whole page.
func TestPartialIsAFragment(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	resp, body := h.get(t, "/partials/contracts?q=CDZZXV")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Contains(t, body, `id="contract-rows"`)
	assert.Contains(t, body, idA)
	assert.NotContains(t, body, "<html", "the partial must not be a full document")
	assert.NotContains(t, body, "<body")
}

func TestContractPage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)
	require.NoError(t, h.reg.Store().UpdateSource(context.Background(), "testnet", idA,
		"https://github.com/soroworks/sorovault", "0123456789abcdef0123456789abcdef01234567"))

	resp, body := h.get(t, "/contracts/"+idA)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Contains(t, body, idA)
	assert.Contains(t, body, `<span class="sig-function">has_voted</span>`)
	assert.Contains(t, body, `<span class="sig-parameter">nullifier</span>`)
	assert.Contains(t, body, `<span class="type-constructor">BytesN</span>`)
	assert.Contains(t, body, `<span class="type-parameter">32</span>`)
	assert.Contains(t, body, `<a class="type-reference" href="#type-VoteParams">VoteParams</a>`)
	assert.Contains(t, body, `<span class="type-definition" id="type-VoteParams">VoteParams</span>`)
	// Every symbol has an anchor of its own, prefixed by kind so a function
	// and a type that share a name do not collide.
	assert.Contains(t, body, `id="fn-has_voted"`)
	assert.Contains(t, body, `href="#fn-has_voted"`)
	assert.Contains(t, body, `id="fn-vote"`)
	assert.Contains(t, body, `id="type-VoteParams"`)
	assert.Contains(t, body, `href="#type-VoteParams"`)
	assert.Contains(t, body, `id="event-Voted"`)
	assert.Contains(t, body, `href="#event-Voted"`)
	assert.Contains(t, body, `class="anchor"`, "each heading carries a visible link to itself")
	assert.Contains(t, body, `<span class="type-keyword">error</span> <span class="type-definition" id="type-Error">Error</span>`)
	assert.Contains(t, body, "Voted", "the event should be listed")
	assert.Contains(t, body, "rssdkver", "build metadata should be shown")
	assert.Contains(t, body, `href="https://github.com/soroworks/sorovault"`)
	assert.Contains(t, body, "0123456789abcdef0123456789abcdef01234567")
	assert.Contains(t, body, "current")
	assert.Contains(t, body, "/api/contracts/"+idA+"/client.ts?", "the typed client should be linked")
}

// TestContractPageEscapesOutput guards against a contract's own strings
// being rendered as markup. Doc comments come from an untrusted chain.
func TestContractPageEscapesOutput(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	_, body := h.get(t, "/contracts/"+idA)

	// Rendered generic types must appear escaped, never as live tags.
	assert.NotContains(t, body, "<BytesN<32>>")
	assert.Contains(t, body, "&lt;")
}

// TestContractPageRendersDocsAsMarkdown proves the documentation is
// rendered as Markdown, and that a doc cannot smuggle markup into the
// page: docs come from an untrusted chain.
func TestContractPageRendersDocsAsMarkdown(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	doc := "### Signals\n\n| idx | meaning |\n|-----|---------|\n| 0   | one     |\n\n" +
		"* first\n* second\n\nCast a vote <script>alert(1)</script> now.\n"

	iface := &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions:  []model.Function{{Name: "vote", Doc: doc}},
		Types: model.Types{
			Structs: []model.Struct{}, Unions: []model.Union{},
			Enums: []model.Enum{}, ErrorEnums: []model.ErrorEnum{},
		},
		Events: []model.Event{},
	}
	require.NoError(t, h.reg.Store().Save(context.Background(), store.Contract{
		ContractID: idA, Network: "testnet", CurrentWasmHash: "hash-a",
	}, iface))

	resp, body := h.get(t, "/contracts/"+idA)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Contains(t, body, "<h3>Signals</h3>", "the heading in the doc should be rendered")
	assert.Contains(t, body, "<table>", "the table in the doc should be rendered")
	assert.Contains(t, body, "<li>first</li>", "the bullet list in the doc should be rendered")

	// The script in the doc must not reach the page as markup.
	assert.NotContains(t, body, "<script>alert",
		"a script in a doc must never be rendered as a tag")

	_, index := h.get(t, "/")
	assert.Equal(t, strings.Count(index, "<script"), strings.Count(body, "<script"),
		"the documentation must not add a script to the page")
}

// TestContractPageShowsProtocolVersion proves the contract page says which
// protocol the contract was built against.
func TestContractPageShowsProtocolVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	_, body := h.get(t, "/contracts/"+idA)

	// The fixture was built against protocol 25.
	assert.Contains(t, body, "<dt>Protocol</dt>")
	assert.Contains(t, body, `<dd class="mono">25</dd>`)
}

// TestContractPageLinksTheWasmHash proves the contract page offers the
// list of the other contracts running the same code.
func TestContractPageLinksTheWasmHash(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	const hash = "c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781"

	_, body := h.get(t, "/contracts/"+idA)
	assert.Contains(t, body, "/api/wasm/"+hash+"/contracts",
		"the WASM hash should link to the contracts that run it")
}

// TestContractPageHasCopyButtons proves the values people paste into other
// tools can be copied.
func TestContractPageHasCopyButtons(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	_, body := h.get(t, "/contracts/"+idA)

	assert.Contains(t, body, `data-copy="#contract-id"`)
	assert.Contains(t, body, `data-copy="#wasm-hash"`)
	assert.Contains(t, body, `data-copy="#sig-vote"`,
		"a function signature should be copyable")
	assert.Contains(t, body, `aria-label="Copy contract ID"`,
		"a button needs a name a screen reader can read out")

	resp, script := h.get(t, "/static/copy.js")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, script, "navigator.clipboard")
	assert.Contains(t, script, `aria-live`, "the copy has to be announced")
}

// TestCopyButtonsNeedJavaScript proves the buttons stay hidden without
// JavaScript, so the values they copy remain plain, selectable text.
func TestCopyButtonsNeedJavaScript(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	resp, css := h.get(t, "/static/app.css")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, css, ".copy { display: none; }",
		"the buttons must be hidden until the script that handles them runs")
	assert.Contains(t, css, "html.js .copy",
		"the script marks the document, and that is what reveals the buttons")
}

// TestContractPageEscapesAnchorIDs proves a contract's own name cannot break
// out of the id it is written into.
func TestContractPageEscapesAnchorIDs(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	iface := &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions:  []model.Function{{Name: `evil"><script>alert(1)</script>`}},
	}
	require.NoError(t, h.reg.Store().Save(context.Background(), store.Contract{
		ContractID:      idA,
		Network:         "testnet",
		CurrentWasmHash: "hash-a",
	}, iface))

	_, body := h.get(t, "/contracts/"+idA)

	assert.NotContains(t, body, "<script>")
	assert.Contains(t, body, "fn-evil", "the anchor is still rendered, escaped")
	assert.Contains(t, body, "&#34;", "the quote in the name is escaped in the attribute")
}

func TestContractPageNotFound(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	resp, body := h.get(t, "/contracts/"+idA)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, body, "No such contract")
	assert.Contains(t, body, "404")
}

func TestContractPageOldVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	// The fixture's real hash, requested explicitly rather than implied.
	const hash = "c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781"

	resp, body := h.get(t, "/contracts/"+idA+"?wasm_hash="+hash)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, hash)

	resp, _ = h.get(t, "/contracts/"+idA+"?wasm_hash=deadbeef")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestIndexHasRegisterForm(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, body := h.get(t, "/")
	assert.Contains(t, body, `action="/contracts"`, "the form works without JavaScript")
	assert.Contains(t, body, `method="post"`)
	assert.Contains(t, body, `name="contract_id"`)
	assert.Contains(t, body, `hx-post="/contracts"`, "the form is enhanced with htmx")
}

func TestRegisterFormRegistersAndRedirects(t *testing.T) {
	t.Parallel()

	t.Run("origin header", func(t *testing.T) {
		h := newHarness(t)
		h.deploy(t, idA)

		resp, _ := h.postForm(t, "/contracts", url.Values{"contract_id": {idA}},
			map[string]string{"Origin": h.server.URL})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, "/contracts/"+idA+"?network=testnet", resp.Header.Get("Location"))

		resp, body := h.get(t, "/contracts/"+idA)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, idA)
	})

	t.Run("referer fallback", func(t *testing.T) {
		h := newHarness(t)
		h.deploy(t, idA)

		resp, _ := h.postForm(t, "/contracts", url.Values{"contract_id": {idA}},
			map[string]string{"Referer": h.server.URL + "/"})
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	})
}

// TestRegisterFormRejectsCrossSite proves the write is guarded: a browser
// names the initiating origin, and a page cannot forge it.
func TestRegisterFormRejectsCrossSite(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.deploy(t, idA)

	for name, headers := range map[string]map[string]string{
		"foreign origin": {"Origin": "https://evil.example"},
		"no source":      {},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := h.postForm(t, "/contracts", url.Values{"contract_id": {idA}}, headers)
			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
			assert.Contains(t, body, "must be submitted from SoroVault")
		})
	}

	_, err := h.reg.Store().GetContract(context.Background(), "testnet", idA)
	assert.ErrorIs(t, err, store.ErrNotFound, "a rejected post must not register anything")
}

// TestRegisterFormErrorsAreInline checks the errors carry the same meaning as
// the API's status codes, and land on the page rather than in a JSON body.
func TestRegisterFormErrorsAreInline(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	t.Run("empty ID", func(t *testing.T) {
		headers := map[string]string{"Origin": h.server.URL}
		resp, body := h.postForm(t, "/contracts", url.Values{"contract_id": {""}}, headers)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, body, "Enter a contract ID")
	})

	t.Run("invalid ID", func(t *testing.T) {
		headers := map[string]string{"Origin": h.server.URL}
		resp, body := h.postForm(t, "/contracts", url.Values{"contract_id": {"not-a-contract"}}, headers)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, body, "not a valid contract ID")
		assert.Contains(t, body, "Registered contracts", "the page comes back without scripting")
		assert.Contains(t, body, `value="not-a-contract"`, "the typed ID is kept for correction")
	})

	t.Run("not found", func(t *testing.T) {
		headers := map[string]string{"Origin": h.server.URL}
		resp, body := h.postForm(t, "/contracts", url.Values{"contract_id": {idB}}, headers)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Contains(t, body, "No contract with that ID")
	})
}

func TestRegisterFormHtmx(t *testing.T) {
	t.Parallel()

	t.Run("error swaps the form", func(t *testing.T) {
		h := newHarness(t)
		headers := map[string]string{"Origin": h.server.URL, "HX-Request": "true"}

		resp, body := h.postForm(t, "/contracts", url.Values{"contract_id": {"nope"}}, headers)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "htmx only swaps successful responses")
		assert.Contains(t, body, `id="register-form"`)
		assert.Contains(t, body, "not a valid contract ID")
		assert.NotContains(t, body, "<html", "the response is a fragment, not a page")
	})

	t.Run("success redirects through the header", func(t *testing.T) {
		h := newHarness(t)
		h.deploy(t, idA)
		headers := map[string]string{"Origin": h.server.URL, "HX-Request": "true"}

		resp, _ := h.postForm(t, "/contracts", url.Values{"contract_id": {idA}}, headers)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "/contracts/"+idA+"?network=testnet", resp.Header.Get("HX-Redirect"))
	})
}

func TestStaticAssets(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// The UI vendors htmx rather than loading it from a CDN, so it must
	// actually be served.
	for _, path := range []string{"/static/app.css", "/static/htmx.min.js"} {
		resp, body := h.get(t, path)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "expected %s to be served", path)
		assert.NotEmpty(t, body)
	}
}

func TestNoExternalResources(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	for _, path := range []string{"/", "/contracts/" + idA} {
		_, body := h.get(t, path)
		assert.NotContains(t, body, "https://unpkg.com",
			"%s must not load scripts from a CDN", path)
		assert.NotContains(t, body, "cdn.jsdelivr.net", "%s must not load scripts from a CDN", path)
	}
}

// TestSecurityHeaders checks the hardening every UI response carries: a strict
// Content-Security-Policy, nosniff, and a frame-ancestors/X-Frame-Options
// policy. These apply to the fragments and static assets too, not just pages.
func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	for _, path := range []string{"/", "/contracts/" + idA, "/partials/contracts", "/static/app.css"} {
		resp, _ := h.get(t, path)

		csp := resp.Header.Get("Content-Security-Policy")
		require.NotEmpty(t, csp, "%s must carry a CSP", path)
		assert.Contains(t, csp, "default-src 'self'", path)
		assert.Contains(t, csp, "frame-ancestors 'none'", path)
		// The UI vendors its only script and stylesheet, so nothing has a
		// reason to relax the policy to inline or eval.
		assert.NotContains(t, csp, "unsafe-inline", path)
		assert.NotContains(t, csp, "unsafe-eval", path)

		assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"), path)
		assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"), path)
	}
}

// TestNoInlineScriptOrStyle keeps the strict CSP honest. The policy allows no
// 'unsafe-inline', so an inline script, style or event handler added later
// would be silently blocked at runtime; this fails the build instead.
func TestNoInlineScriptOrStyle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.register(t, idA)

	for _, path := range []string{"/", "/contracts/" + idA, "/partials/contracts"} {
		_, body := h.get(t, path)
		assert.NotContains(t, body, "<style", "%s must not embed a stylesheet", path)
		assert.NotContains(t, body, " style=", "%s must not use a style attribute", path)
		assert.NotContains(t, body, "<script>", "%s must not embed inline script", path)
		assert.NotContains(t, body, "onclick=", "%s must not use inline event handlers", path)
	}
}

// TestHtmxInjectsNoInlineStyle guards the one inline style htmx adds on its
// own: its default loading-indicator stylesheet. The UI disables it through a
// meta htmx-config, because an injected <style> would violate the CSP.
func TestHtmxInjectsNoInlineStyle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, body := h.get(t, "/")
	assert.Contains(t, body, `name="htmx-config"`)
	assert.Contains(t, body, "includeIndicatorStyles")
}
