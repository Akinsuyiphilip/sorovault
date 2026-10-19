package spec_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/wasm"
	"github.com/soroworks/sorovault/internal/wasm/wasmtest"
)

// decodeSection decodes hand-encoded entries the way production does: the
// entries are concatenated into a "contractspecv0" section, that section is
// wrapped in a module, and the module goes through XDRDecoder. Going through
// the real path matters here, because the stream is undelimited and a decoder
// that mis-framed it could reorder or drop entries before appendEntry ever ran.
func decodeSection(t *testing.T, entries ...xdr.ScSpecEntry) *model.Interface {
	t.Helper()

	module := wasmtest.Module(wasmtest.Section{
		Name: wasm.SectionContractSpec,
		Body: encodeEntries(t, entries...),
	})

	iface, err := spec.XDRDecoder{}.Decode(module)
	require.NoError(t, err)
	return iface
}

// The UDT entries below carry only a name: ordering is what this file tests,
// and every field would just add noise. Each kind still has to survive the
// round trip through XDR, so the empty field lists are encoded for real.

func structEntry(name string) xdr.ScSpecEntry {
	return xdr.ScSpecEntry{
		Kind:        xdr.ScSpecEntryKindScSpecEntryUdtStructV0,
		UdtStructV0: &xdr.ScSpecUdtStructV0{Name: name},
	}
}

func enumEntry(name string) xdr.ScSpecEntry {
	return xdr.ScSpecEntry{
		Kind:      xdr.ScSpecEntryKindScSpecEntryUdtEnumV0,
		UdtEnumV0: &xdr.ScSpecUdtEnumV0{Name: name},
	}
}

func errorEnumEntry(name string) xdr.ScSpecEntry {
	return xdr.ScSpecEntry{
		Kind:           xdr.ScSpecEntryKindScSpecEntryUdtErrorEnumV0,
		UdtErrorEnumV0: &xdr.ScSpecUdtErrorEnumV0{Name: name},
	}
}

func unionEntry(name string) xdr.ScSpecEntry {
	return xdr.ScSpecEntry{
		Kind:       xdr.ScSpecEntryKindScSpecEntryUdtUnionV0,
		UdtUnionV0: &xdr.ScSpecUdtUnionV0{Name: name},
	}
}

func eventEntry(name string) xdr.ScSpecEntry {
	return xdr.ScSpecEntry{
		Kind:    xdr.ScSpecEntryKindScSpecEntryEventV0,
		EventV0: &xdr.ScSpecEventV0{Name: xdr.ScSymbol(name)},
	}
}

// TestEntryOrderingFollowsTheSpec pins the order of every bucket in the
// published ABI. The ABI JSON is a public contract: a consumer that caches it
// or generates a typed binding must see the same sequence on every run, so
// functions and types have to come out in the order the spec declares them —
// not sorted, not map-ordered, not deduplicated.
func TestEntryOrderingFollowsTheSpec(t *testing.T) {
	t.Parallel()

	// Four interleaved functions plus two members of every other bucket, so
	// reordering anywhere in appendEntry fails this test rather than quietly
	// changing the published ABI.
	iface := decodeSection(t,
		functionEntry("alpha", nil, nil),
		structEntry("S1"),
		functionEntry("beta", nil, nil),
		enumEntry("E1"),
		functionEntry("gamma", nil, nil),
		errorEnumEntry("EE1"),
		structEntry("S2"),
		unionEntry("U1"),
		eventEntry("Ev1"),
		functionEntry("delta", nil, nil),
		errorEnumEntry("EE2"),
		unionEntry("U2"),
		eventEntry("Ev2"),
		enumEntry("E2"),
	)

	require.Len(t, iface.Functions, 4)
	assert.Equal(t, "alpha", iface.Functions[0].Name)
	assert.Equal(t, "beta", iface.Functions[1].Name)
	assert.Equal(t, "gamma", iface.Functions[2].Name)
	assert.Equal(t, "delta", iface.Functions[3].Name)

	require.Len(t, iface.Types.Structs, 2)
	assert.Equal(t, "S1", iface.Types.Structs[0].Name)
	assert.Equal(t, "S2", iface.Types.Structs[1].Name)

	require.Len(t, iface.Types.Enums, 2)
	assert.Equal(t, "E1", iface.Types.Enums[0].Name)
	assert.Equal(t, "E2", iface.Types.Enums[1].Name)

	require.Len(t, iface.Types.ErrorEnums, 2)
	assert.Equal(t, "EE1", iface.Types.ErrorEnums[0].Name)
	assert.Equal(t, "EE2", iface.Types.ErrorEnums[1].Name)

	require.Len(t, iface.Types.Unions, 2)
	assert.Equal(t, "U1", iface.Types.Unions[0].Name)
	assert.Equal(t, "U2", iface.Types.Unions[1].Name)

	require.Len(t, iface.Events, 2)
	assert.Equal(t, "Ev1", iface.Events[0].Name)
	assert.Equal(t, "Ev2", iface.Events[1].Name)
}

// TestDuplicateEntryNamesAreDeterministic documents what happens when a spec
// declares the same name twice — something a buggy SDK could emit. Both
// entries are kept in declaration order and a lookup returns the first, so the
// answer never depends on map iteration order: decoding the same section twice
// yields the identical interface.
func TestDuplicateEntryNamesAreDeterministic(t *testing.T) {
	t.Parallel()

	entries := []xdr.ScSpecEntry{
		functionEntry("dup", nil, nil),
		structEntry("Dup"),
		functionEntry("dup", nil, nil),
		structEntry("Dup"),
	}

	first := decodeSection(t, entries...)
	second := decodeSection(t, entries...)

	require.Len(t, first.Functions, 2, "a duplicate name must not be dropped")
	assert.Equal(t, "dup", first.Functions[0].Name)
	assert.Equal(t, "dup", first.Functions[1].Name)

	require.Len(t, first.Types.Structs, 2, "a duplicate name must not be dropped")
	assert.Equal(t, "Dup", first.Types.Structs[0].Name)
	assert.Equal(t, "Dup", first.Types.Structs[1].Name)

	// Interface.Function scans in declaration order, so a consumer resolving
	// the name gets the first declaration every time.
	fn, ok := first.Function("dup")
	require.True(t, ok)
	assert.Equal(t, "dup", fn.Name)

	assert.Equal(t, first, second,
		"decoding the same section must yield the same interface on every run")
}
