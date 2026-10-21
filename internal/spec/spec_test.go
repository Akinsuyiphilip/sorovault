package spec_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/wasm"
	"github.com/soroworks/sorovault/internal/wasm/wasmtest"
)

var update = flag.Bool("update", false, "rewrite the golden ABI file from the fixture")

// encodeEntries encodes spec entries the way a Soroban build does: back to
// back with no framing between them.
func encodeEntries(t *testing.T, entries ...xdr.ScSpecEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	for i := range entries {
		raw, err := entries[i].MarshalBinary()
		require.NoError(t, err)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func encodeMeta(t *testing.T, kv map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	for k, v := range kv {
		entry := xdr.ScMetaEntry{
			Kind: xdr.ScMetaKindScMetaV0,
			V0:   &xdr.ScMetaV0{Key: k, Val: v},
		}
		raw, err := entry.MarshalBinary()
		require.NoError(t, err)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// encodeEnvMeta encodes an interface-version entry the way a Soroban build
// writes it into the "contractenvmetav0" section.
func encodeEnvMeta(t *testing.T, protocol, preRelease uint32) []byte {
	t.Helper()

	entry := xdr.ScEnvMetaEntry{
		Kind: xdr.ScEnvMetaKindScEnvMetaKindInterfaceVersion,
		InterfaceVersion: &xdr.ScEnvMetaEntryInterfaceVersion{
			Protocol:   xdr.Uint32(protocol),
			PreRelease: xdr.Uint32(preRelease),
		},
	}
	raw, err := entry.MarshalBinary()
	require.NoError(t, err)
	return raw
}

func functionEntry(name string, inputs []xdr.ScSpecFunctionInputV0, outputs []xdr.ScSpecTypeDef) xdr.ScSpecEntry {
	return xdr.ScSpecEntry{
		Kind: xdr.ScSpecEntryKindScSpecEntryFunctionV0,
		FunctionV0: &xdr.ScSpecFunctionV0{
			Name:    xdr.ScSymbol(name),
			Inputs:  inputs,
			Outputs: outputs,
		},
	}
}

func TestReadEntries(t *testing.T) {
	t.Parallel()

	section := encodeEntries(t,
		functionEntry("a", nil, nil),
		functionEntry("b", nil, []xdr.ScSpecTypeDef{def(xdr.ScSpecTypeScSpecTypeU32)}),
		functionEntry("c", nil, nil),
	)

	entries, err := spec.ReadEntries(section)
	require.NoError(t, err)
	require.Len(t, entries, 3, "the undelimited stream must yield every entry")
	assert.Equal(t, xdr.ScSymbol("a"), entries[0].FunctionV0.Name)
	assert.Equal(t, xdr.ScSymbol("c"), entries[2].FunctionV0.Name)
}

func TestReadEntriesErrors(t *testing.T) {
	t.Parallel()

	valid := encodeEntries(t, functionEntry("a", nil, nil))

	t.Run("empty section yields no entries", func(t *testing.T) {
		t.Parallel()

		entries, err := spec.ReadEntries(nil)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("truncated stream is an error", func(t *testing.T) {
		t.Parallel()

		_, err := spec.ReadEntries(valid[:len(valid)-3])
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decoding entry")
	})

	t.Run("trailing garbage is an error", func(t *testing.T) {
		t.Parallel()

		// Trailing bytes mean the section was misread, not that the
		// contract has one extra entry to ignore.
		_, err := spec.ReadEntries(append(append([]byte{}, valid...), 0xFF, 0xFF))
		require.Error(t, err)
	})
}

func TestFromEntries(t *testing.T) {
	t.Parallel()

	entries := []xdr.ScSpecEntry{
		functionEntry("transfer",
			[]xdr.ScSpecFunctionInputV0{
				{Name: "from", Type: def(xdr.ScSpecTypeScSpecTypeAddress)},
				{Name: "amount", Doc: "how much", Type: def(xdr.ScSpecTypeScSpecTypeI128)},
			},
			[]xdr.ScSpecTypeDef{def(xdr.ScSpecTypeScSpecTypeVoid)},
		),
		{
			Kind: xdr.ScSpecEntryKindScSpecEntryUdtStructV0,
			UdtStructV0: &xdr.ScSpecUdtStructV0{
				Name: "Balance", Doc: "a balance", Lib: "token",
				Fields: []xdr.ScSpecUdtStructFieldV0{
					{Name: "owner", Type: def(xdr.ScSpecTypeScSpecTypeAddress)},
					{Name: "amount", Type: def(xdr.ScSpecTypeScSpecTypeI128)},
				},
			},
		},
		{
			Kind: xdr.ScSpecEntryKindScSpecEntryUdtEnumV0,
			UdtEnumV0: &xdr.ScSpecUdtEnumV0{
				Name: "Status",
				Cases: []xdr.ScSpecUdtEnumCaseV0{
					{Name: "Active", Value: 0},
					{Name: "Frozen", Value: 7},
				},
			},
		},
		{
			Kind: xdr.ScSpecEntryKindScSpecEntryUdtErrorEnumV0,
			UdtErrorEnumV0: &xdr.ScSpecUdtErrorEnumV0{
				Name:  "Error",
				Cases: []xdr.ScSpecUdtErrorEnumCaseV0{{Name: "NotAuthorized", Value: 1}},
			},
		},
	}

	iface, err := spec.FromEntries(entries)
	require.NoError(t, err)

	require.Len(t, iface.Functions, 1)
	fn := iface.Functions[0]
	assert.Equal(t, "transfer", fn.Name)
	assert.Equal(t, "fn transfer(from: Address, amount: i128) -> ()", fn.Signature())
	assert.Equal(t, "how much", fn.Inputs[1].Doc)

	require.Len(t, iface.Types.Structs, 1)
	assert.Equal(t, "Balance", iface.Types.Structs[0].Name)
	assert.Equal(t, "token", iface.Types.Structs[0].Lib)
	assert.False(t, iface.Types.Structs[0].IsTuple)

	require.Len(t, iface.Types.Enums, 1)
	assert.Equal(t, uint32(7), iface.Types.Enums[0].Cases[1].Value)

	require.Len(t, iface.Types.ErrorEnums, 1)
	assert.Equal(t, "NotAuthorized", iface.Types.ErrorEnums[0].Cases[0].Name)

	assert.Equal(t, model.ABIVersion, iface.ABIVersion)
}

func TestFromEntriesUnions(t *testing.T) {
	t.Parallel()

	entries := []xdr.ScSpecEntry{{
		Kind: xdr.ScSpecEntryKindScSpecEntryUdtUnionV0,
		UdtUnionV0: &xdr.ScSpecUdtUnionV0{
			Name: "DataKey",
			Cases: []xdr.ScSpecUdtUnionCaseV0{
				{
					Kind:     xdr.ScSpecUdtUnionCaseV0KindScSpecUdtUnionCaseVoidV0,
					VoidCase: &xdr.ScSpecUdtUnionCaseVoidV0{Name: "Admin", Doc: "the admin"},
				},
				{
					Kind: xdr.ScSpecUdtUnionCaseV0KindScSpecUdtUnionCaseTupleV0,
					TupleCase: &xdr.ScSpecUdtUnionCaseTupleV0{
						Name: "Balance",
						Type: []xdr.ScSpecTypeDef{
							def(xdr.ScSpecTypeScSpecTypeAddress),
							def(xdr.ScSpecTypeScSpecTypeU32),
						},
					},
				},
			},
		},
	}}

	iface, err := spec.FromEntries(entries)
	require.NoError(t, err)

	require.Len(t, iface.Types.Unions, 1)
	cases := iface.Types.Unions[0].Cases
	require.Len(t, cases, 2)

	assert.Equal(t, "Admin", cases[0].Name)
	assert.Equal(t, "the admin", cases[0].Doc)
	assert.Empty(t, cases[0].Values, "a void case carries no payload")

	assert.Equal(t, "Balance", cases[1].Name)
	require.Len(t, cases[1].Values, 2)
	assert.Equal(t, "Address", cases[1].Values[0].Display)
	assert.Equal(t, "u32", cases[1].Values[1].Display)
}

func TestFromEntriesEvents(t *testing.T) {
	t.Parallel()

	entries := []xdr.ScSpecEntry{{
		Kind: xdr.ScSpecEntryKindScSpecEntryEventV0,
		EventV0: &xdr.ScSpecEventV0{
			Name:         "Transfer",
			Lib:          "token",
			PrefixTopics: []xdr.ScSymbol{"transfer"},
			DataFormat:   xdr.ScSpecEventDataFormatScSpecEventDataFormatMap,
			Params: []xdr.ScSpecEventParamV0{
				{
					Name:     "from",
					Type:     def(xdr.ScSpecTypeScSpecTypeAddress),
					Location: xdr.ScSpecEventParamLocationV0ScSpecEventParamLocationTopicList,
				},
				{
					Name:     "amount",
					Type:     def(xdr.ScSpecTypeScSpecTypeI128),
					Location: xdr.ScSpecEventParamLocationV0ScSpecEventParamLocationData,
				},
			},
		},
	}}

	iface, err := spec.FromEntries(entries)
	require.NoError(t, err)

	require.Len(t, iface.Events, 1)
	ev := iface.Events[0]
	assert.Equal(t, "Transfer", ev.Name)
	assert.Equal(t, []string{"transfer"}, ev.Prefix)
	assert.Equal(t, "map", ev.DataFormat)
	assert.Equal(t, "topic-list", ev.Params[0].Location)
	assert.Equal(t, "data", ev.Params[1].Location)
}

// TestFromEntriesTupleStruct covers Soroban encoding a Rust tuple struct as
// a struct whose fields are named by their position.
func TestFromEntriesTupleStruct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fields  []string
		isTuple bool
	}{
		{"positional fields", []string{"0", "1"}, true},
		{"single positional field", []string{"0"}, true},
		{"named fields", []string{"owner", "amount"}, false},
		{"out of order indices", []string{"1", "0"}, false},
		{"indices not starting at zero", []string{"1", "2"}, false},
		{"no fields is a unit struct", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fields := make([]xdr.ScSpecUdtStructFieldV0, 0, len(tt.fields))
			for _, name := range tt.fields {
				fields = append(fields, xdr.ScSpecUdtStructFieldV0{
					Name: name, Type: def(xdr.ScSpecTypeScSpecTypeU32),
				})
			}

			iface, err := spec.FromEntries([]xdr.ScSpecEntry{{
				Kind:        xdr.ScSpecEntryKindScSpecEntryUdtStructV0,
				UdtStructV0: &xdr.ScSpecUdtStructV0{Name: "T", Fields: fields},
			}})
			require.NoError(t, err)
			assert.Equal(t, tt.isTuple, iface.Types.Structs[0].IsTuple)
		})
	}
}

// TestFromEntriesUnknownKind checks that an entry kind this build predates
// is skipped, leaving the rest of the interface usable.
func TestFromEntriesUnknownKind(t *testing.T) {
	t.Parallel()

	iface, err := spec.FromEntries([]xdr.ScSpecEntry{
		{Kind: xdr.ScSpecEntryKind(9999)},
		functionEntry("still_here", nil, nil),
	})
	require.NoError(t, err)

	require.Len(t, iface.Functions, 1)
	assert.Equal(t, "still_here", iface.Functions[0].Name)
}

func TestFromEntriesEmpty(t *testing.T) {
	t.Parallel()

	iface, err := spec.FromEntries(nil)
	require.NoError(t, err)

	// Empty rather than nil, so the JSON encodes as [] and consumers can
	// iterate without a nil check.
	assert.NotNil(t, iface.Functions)
	assert.NotNil(t, iface.Events)
	assert.NotNil(t, iface.Types.Structs)
}

func TestDecodeModule(t *testing.T) {
	t.Parallel()

	section := encodeEntries(t, functionEntry("hello", nil, []xdr.ScSpecTypeDef{def(xdr.ScSpecTypeScSpecTypeSymbol)}))
	meta := encodeMeta(t, map[string]string{"rsver": "1.93.0", "rssdkver": "25.3.0"})

	module := wasmtest.Module(
		wasmtest.Section{Name: wasm.SectionContractSpec, Body: section},
		wasmtest.Section{Name: wasm.SectionContractMeta, Body: meta},
	)

	iface, err := spec.XDRDecoder{}.Decode(module)
	require.NoError(t, err)

	require.Len(t, iface.Functions, 1)
	assert.Equal(t, "hello", iface.Functions[0].Name)
	assert.Equal(t, map[string]string{"rsver": "1.93.0", "rssdkver": "25.3.0"}, iface.Meta)
}

func TestDecodeModuleWithoutMeta(t *testing.T) {
	t.Parallel()

	// Metadata is optional; its absence must not fail the decode.
	module := wasmtest.Module(wasmtest.Section{
		Name: wasm.SectionContractSpec,
		Body: encodeEntries(t, functionEntry("hello", nil, nil)),
	})

	iface, err := spec.XDRDecoder{}.Decode(module)
	require.NoError(t, err)
	assert.Nil(t, iface.Meta)
	assert.Len(t, iface.Functions, 1)
}

// TestDecodeEnvMeta covers the section that records which host environment
// a contract was built against.
func TestDecodeEnvMeta(t *testing.T) {
	t.Parallel()

	section := encodeEntries(t, functionEntry("hello", nil, nil))

	t.Run("the interface version is decoded", func(t *testing.T) {
		t.Parallel()

		module := wasmtest.Module(
			wasmtest.Section{Name: wasm.SectionContractSpec, Body: section},
			wasmtest.Section{Name: wasm.SectionContractEnvMeta, Body: encodeEnvMeta(t, 25, 0)},
		)

		iface, err := spec.XDRDecoder{}.Decode(module)
		require.NoError(t, err)
		require.NotNil(t, iface.EnvMeta)
		assert.Equal(t, uint32(25), iface.EnvMeta.Protocol)
		assert.Equal(t, uint32(0), iface.EnvMeta.PreRelease)
		assert.Equal(t, "25", iface.EnvMeta.InterfaceVersion)
	})

	t.Run("a pre-release number is kept apart from the protocol", func(t *testing.T) {
		t.Parallel()

		module := wasmtest.Module(
			wasmtest.Section{Name: wasm.SectionContractSpec, Body: section},
			wasmtest.Section{Name: wasm.SectionContractEnvMeta, Body: encodeEnvMeta(t, 25, 3)},
		)

		iface, err := spec.XDRDecoder{}.Decode(module)
		require.NoError(t, err)
		require.NotNil(t, iface.EnvMeta)
		assert.Equal(t, uint32(25), iface.EnvMeta.Protocol)
		assert.Equal(t, uint32(3), iface.EnvMeta.PreRelease)
		assert.Equal(t, "25.3", iface.EnvMeta.InterfaceVersion)
	})

	t.Run("a contract without the section has no environment metadata", func(t *testing.T) {
		t.Parallel()

		module := wasmtest.Module(wasmtest.Section{
			Name: wasm.SectionContractSpec,
			Body: section,
		})

		iface, err := spec.XDRDecoder{}.Decode(module)
		require.NoError(t, err)
		assert.Nil(t, iface.EnvMeta, "the section is optional, so its absence is not an error")
	})
}

// TestDecodeEnvMetaOldPacking proves a section written before the XDR
// split still decodes.
//
// The interface version used to be one 64-bit number that packed the
// protocol version into its high half and the pre-release number into its
// low half. XDR is big-endian, so the high half comes first, and the bytes
// are the ones the two 32-bit fields use today.
func TestDecodeEnvMetaOldPacking(t *testing.T) {
	t.Parallel()

	var body bytes.Buffer
	// The union discriminant, then the packed interface version.
	body.Write([]byte{0, 0, 0, 0})
	for _, half := range []uint32{25, 3} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], half)
		body.Write(b[:])
	}

	module := wasmtest.Module(
		wasmtest.Section{Name: wasm.SectionContractSpec, Body: encodeEntries(t, functionEntry("hello", nil, nil))},
		wasmtest.Section{Name: wasm.SectionContractEnvMeta, Body: body.Bytes()},
	)

	iface, err := spec.XDRDecoder{}.Decode(module)
	require.NoError(t, err)
	require.NotNil(t, iface.EnvMeta)
	assert.Equal(t, uint32(25), iface.EnvMeta.Protocol)
	assert.Equal(t, uint32(3), iface.EnvMeta.PreRelease)
}

func TestDecodeModuleWithoutSpecSection(t *testing.T) {
	t.Parallel()

	module := wasmtest.Module(wasmtest.Section{Name: "producers", Body: []byte("rustc")})

	_, err := spec.XDRDecoder{}.Decode(module)
	require.ErrorIs(t, err, spec.ErrNoSpecSection)
}

func TestDecodeModuleMalformed(t *testing.T) {
	t.Parallel()

	_, err := spec.XDRDecoder{}.Decode([]byte("not a wasm module at all"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, spec.ErrNoSpecSection,
		"a corrupt module is a different failure from a contract with no spec")
}

// TestDecodeFixture decodes a real contract fetched from testnet and
// compares the result against a checked-in golden file, so a change to the
// public ABI shape has to be made deliberately.
func TestDecodeFixture(t *testing.T) {
	t.Parallel()

	const (
		wasmPath   = "testdata/zkvote.wasm"
		goldenPath = "testdata/zkvote.golden.json"
	)

	module, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	iface, err := spec.XDRDecoder{}.Decode(module)
	require.NoError(t, err)

	got, err := json.MarshalIndent(iface, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')

	if *update {
		require.NoError(t, os.WriteFile(filepath.Clean(goldenPath), got, 0o644))
		t.Logf("wrote %s", goldenPath)
	}

	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing; regenerate with -update")

	assert.Equal(t, string(want), string(got),
		"decoded ABI differs from the golden file; if the change is intended, rerun with -update")
}

// TestDecodeFixtureShape asserts the properties that matter about the real
// contract independently of the golden file, so a careless -update cannot
// quietly bless a regression.
func TestDecodeFixtureShape(t *testing.T) {
	t.Parallel()

	module, err := os.ReadFile("testdata/zkvote.wasm")
	require.NoError(t, err)

	iface, err := spec.XDRDecoder{}.Decode(module)
	require.NoError(t, err)

	assert.Equal(t, "25.3.0#dcbea44513feb7734af6b6c4aced2c4a7a2715d0", iface.Meta["rssdkver"])

	// The fixture was built against protocol 25.
	require.NotNil(t, iface.EnvMeta, "the real contract carries an environment metadata section")
	assert.Equal(t, uint32(25), iface.EnvMeta.Protocol)
	assert.Equal(t, uint32(0), iface.EnvMeta.PreRelease)
	assert.Equal(t, "25", iface.EnvMeta.InterfaceVersion)

	require.Len(t, iface.Functions, 7)
	fn, ok := iface.Function("vote")
	require.True(t, ok)
	assert.Equal(t,
		"fn vote(voter: Address, proposal_index: u32, pub_signals: Vec<BytesN<32>>, "+
			"a: BytesN<64>, b: BytesN<128>, c: BytesN<64>) -> Result<(), Error>",
		fn.Signature())
	assert.Contains(t, fn.Doc, "Groth16 proof", "doc comments must survive decoding")

	// A nested container from a real contract, checked structurally rather
	// than only through its rendered form.
	pubSignals := fn.Inputs[2].Type
	require.Equal(t, model.KindVec, pubSignals.Kind)
	require.NotNil(t, pubSignals.Element)
	assert.Equal(t, model.KindBytesN, pubSignals.Element.Kind)
	assert.Equal(t, uint32(32), pubSignals.Element.N)

	require.Len(t, iface.Types.Structs, 2)
	require.Len(t, iface.Types.ErrorEnums, 1)
	assert.Len(t, iface.Types.ErrorEnums[0].Cases, 7)

	require.Len(t, iface.Events, 1)
	assert.Equal(t, "Voted", iface.Events[0].Name)
	assert.Equal(t, "topic-list", iface.Events[0].Params[0].Location)
}

// TestFixtureHashMatchesName guards the fixture's provenance: a contract's
// WASM hash is the SHA-256 of the module, so this is the same check the
// network performs.
func TestFixtureHashMatchesName(t *testing.T) {
	t.Parallel()

	module, err := os.ReadFile("testdata/zkvote.wasm")
	require.NoError(t, err)

	assert.Equal(t,
		"c618dae264864ccf446a3c7db27da80c7c83e840131242cc2fe9cd32a2a20781",
		sha256Hex(module),
		"the fixture no longer matches the on-chain WASM hash recorded in testdata/README.md")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
