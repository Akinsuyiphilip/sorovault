package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

	return &harness{server: srv, fake: fake, reg: reg}
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

	resp, body := h.get(t, "/contracts/"+idA)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Contains(t, body, idA)
	assert.Contains(t, body, "fn has_voted(nullifier: BytesN&lt;32&gt;) -&gt; bool")
	assert.Contains(t, body, "struct VoteParams")
	assert.Contains(t, body, "error Error")
	assert.Contains(t, body, "Voted", "the event should be listed")
	assert.Contains(t, body, "rssdkver", "build metadata should be shown")
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
