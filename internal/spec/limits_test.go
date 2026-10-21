package spec_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/wasm"
	"github.com/soroworks/sorovault/internal/wasm/wasmtest"
)

// TestDecodeLimitModuleSize proves a module past the size bound is rejected
// before any section is walked.
func TestDecodeLimitModuleSize(t *testing.T) {
	t.Parallel()

	module := wasmtest.Module(wasmtest.Section{
		Name: wasm.SectionContractSpec,
		Body: encodeEntries(t, functionEntry("hello", nil, nil)),
	})
	module = append(module, make([]byte, wasm.MaxModuleSize)...)
	require.Greater(t, len(module), wasm.MaxModuleSize)

	_, err := spec.XDRDecoder{}.Decode(module)
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrLimitExceeded)
	assert.ErrorIs(t, err, wasm.ErrModuleTooLarge)
	assert.Contains(t, err.Error(), "limit")
}

// TestDecodeLimitSpecSectionSize proves a contractspecv0 section past its
// own bound is rejected before the XDR stream is decoded.
func TestDecodeLimitSpecSectionSize(t *testing.T) {
	t.Parallel()

	section := make([]byte, spec.MaxSpecSectionSize+1)
	module := wasmtest.Module(wasmtest.Section{Name: wasm.SectionContractSpec, Body: section})
	require.Less(t, len(module), wasm.MaxModuleSize,
		"the section limit must bite before the module limit does")

	_, err := spec.XDRDecoder{}.Decode(module)
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrLimitExceeded)
	assert.Contains(t, err.Error(), "spec section")
}

// TestDecodeLimitSpecEntries proves a section made of many tiny entries
// stops at the entry count rather than decoding all of them.
func TestDecodeLimitSpecEntries(t *testing.T) {
	t.Parallel()

	entries := make([]xdr.ScSpecEntry, 0, spec.MaxSpecEntries+1)
	for range spec.MaxSpecEntries + 1 {
		entries = append(entries, functionEntry("f", nil, nil))
	}
	section := encodeEntries(t, entries...)
	require.Less(t, len(section), spec.MaxSpecSectionSize,
		"the entry limit must bite before the section limit does")

	_, err := spec.XDRDecoder{}.Decode(wasmtest.Module(
		wasmtest.Section{Name: wasm.SectionContractSpec, Body: section},
	))
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrLimitExceeded)
	assert.Contains(t, err.Error(), "spec entries")
}

// TestDecodeLimitTypeDepth proves nesting is bounded during conversion from
// XDR, not after a nested model value has already been built.
func TestDecodeLimitTypeDepth(t *testing.T) {
	t.Parallel()

	nested := def(xdr.ScSpecTypeScSpecTypeU32)
	for range spec.MaxTypeDepth + 2 {
		nested = vecOf(nested)
	}

	entry := xdr.ScSpecEntry{
		Kind: xdr.ScSpecEntryKindScSpecEntryUdtStructV0,
		UdtStructV0: &xdr.ScSpecUdtStructV0{
			Name:   "DeeplyNested",
			Fields: []xdr.ScSpecUdtStructFieldV0{{Name: "inner", Type: nested}},
		},
	}

	_, err := spec.XDRDecoder{}.Decode(wasmtest.Module(wasmtest.Section{
		Name: wasm.SectionContractSpec,
		Body: encodeEntries(t, entry),
	}))
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrLimitExceeded)
	assert.Contains(t, err.Error(), "nested deeper than")
}

// TestFromEntriesLimit proves the entry bound also holds for callers that
// already have decoded XDR entries, without going through a module.
func TestFromEntriesLimit(t *testing.T) {
	t.Parallel()

	entries := make([]xdr.ScSpecEntry, spec.MaxSpecEntries+1)
	_, err := spec.FromEntries(entries)
	require.Error(t, err)
	assert.ErrorIs(t, err, spec.ErrLimitExceeded)
}
