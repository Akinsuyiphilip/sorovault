package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/wasm"
	"github.com/soroworks/sorovault/internal/wasm/wasmtest"
)

// TestDecodeLimitsAreUnprocessable proves an oversized module or spec
// section is a property of the contract, not a server fault, so registration
// (and cataloguing) reports 422 with the limit rather than 500.
func TestDecodeLimitsAreUnprocessable(t *testing.T) {
	t.Parallel()

	t.Run("spec section over the limit", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		module := wasmtest.Module(wasmtest.Section{
			Name: wasm.SectionContractSpec,
			Body: make([]byte, spec.MaxSpecSectionSize+1),
		})
		h.fake.Deploy(idA, module)

		resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(raw))
		assert.Contains(t, h.decode(t, raw)["error"], "spec section")
	})

	t.Run("module over the limit", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		// A structurally valid module padded past the limit: the size check
		// has to reject it before walking any sections.
		module := wasmtest.Module(wasmtest.Section{
			Name: wasm.SectionContractSpec,
			Body: []byte{},
		})
		module = append(module, make([]byte, wasm.MaxModuleSize)...)
		require.Greater(t, len(module), wasm.MaxModuleSize)
		h.fake.Deploy(idA, module)

		resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(raw))
		assert.Contains(t, h.decode(t, raw)["error"], "limit")
	})

	t.Run("cataloguing over the limit", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		hash := h.fake.UploadWasm(wasmtest.Module(wasmtest.Section{
			Name: wasm.SectionContractSpec,
			Body: make([]byte, spec.MaxSpecSectionSize+1),
		}))

		resp, raw := h.do(t, http.MethodPost, "/api/wasm/"+hash, "")
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(raw))
		assert.Contains(t, h.decode(t, raw)["error"], "spec section")
	})
}
