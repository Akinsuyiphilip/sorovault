package model_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/model"
)

func ptr(t model.Type) *model.Type { return &t }

func TestRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   model.Type
		want string
	}{
		{"scalar u32", model.Scalar(model.KindU32), "u32"},
		{"scalar void", model.Scalar(model.KindVoid), "()"},
		{"scalar address", model.Scalar(model.KindAddress), "Address"},
		{"scalar muxed address", model.Scalar(model.KindMuxedAddress), "MuxedAddress"},
		{
			name: "vec of u32",
			in:   model.Type{Kind: model.KindVec, Element: ptr(model.Scalar(model.KindU32))},
			want: "Vec<u32>",
		},
		{
			name: "option of vec",
			in: model.Type{Kind: model.KindOption, Inner: ptr(model.Type{
				Kind: model.KindVec, Element: ptr(model.Scalar(model.KindU32)),
			})},
			want: "Option<Vec<u32>>",
		},
		{
			name: "map",
			in: model.Type{
				Kind:  model.KindMap,
				Key:   ptr(model.Scalar(model.KindSymbol)),
				Value: ptr(model.Scalar(model.KindI128)),
			},
			want: "Map<Symbol, i128>",
		},
		{
			name: "result",
			in: model.Type{
				Kind:  model.KindResult,
				Ok:    ptr(model.Scalar(model.KindVoid)),
				Error: ptr(model.Scalar(model.KindError)),
			},
			want: "Result<(), Error>",
		},
		{
			name: "bytesN",
			in:   model.Type{Kind: model.KindBytesN, N: 32},
			want: "BytesN<32>",
		},
		{
			name: "udt",
			in:   model.Type{Kind: model.KindUDT, Name: "Balance"},
			want: "Balance",
		},
		{
			name: "empty tuple",
			in:   model.Type{Kind: model.KindTuple},
			want: "()",
		},
		{
			name: "tuple",
			in: model.Type{Kind: model.KindTuple, Elements: []model.Type{
				model.Scalar(model.KindAddress),
				model.Scalar(model.KindU64),
			}},
			want: "(Address, u64)",
		},
		{
			name: "deeply nested",
			in: model.Type{Kind: model.KindMap,
				Key: ptr(model.Scalar(model.KindSymbol)),
				Value: ptr(model.Type{Kind: model.KindVec, Element: ptr(model.Type{
					Kind: model.KindOption, Inner: ptr(model.Type{Kind: model.KindBytesN, N: 4}),
				})}),
			},
			want: "Map<Symbol, Vec<Option<BytesN<4>>>>",
		},
		{
			// A container missing its child should still render rather
			// than panic, since the model is public and hand-buildable.
			name: "vec with nil element",
			in:   model.Type{Kind: model.KindVec},
			want: "Vec<?>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, model.Render(tt.in))
		})
	}
}

func TestScalarPanicsOnComposite(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() { model.Scalar(model.KindVec) },
		"Scalar must reject composite kinds rather than return a type with no Display")
}

func TestSignature(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fn   model.Function
		want string
	}{
		{
			name: "no args, no return",
			fn:   model.Function{Name: "reset"},
			want: "fn reset()",
		},
		{
			name: "args and return",
			fn: model.Function{
				Name: "transfer",
				Inputs: []model.Param{
					{Name: "from", Type: model.Scalar(model.KindAddress)},
					{Name: "to", Type: model.Scalar(model.KindAddress)},
					{Name: "amount", Type: model.Scalar(model.KindI128)},
				},
				Outputs: []model.Type{model.Scalar(model.KindVoid)},
			},
			want: "fn transfer(from: Address, to: Address, amount: i128) -> ()",
		},
		{
			name: "nested types render inline",
			fn: model.Function{
				Name:   "balances",
				Inputs: []model.Param{{Name: "who", Type: model.Scalar(model.KindAddress)}},
				Outputs: []model.Type{{
					Kind: model.KindVec, Element: ptr(model.Scalar(model.KindI128)),
				}},
			},
			want: "fn balances(who: Address) -> Vec<i128>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.fn.Signature())
		})
	}
}

func TestInterfaceFunction(t *testing.T) {
	t.Parallel()

	iface := &model.Interface{Functions: []model.Function{
		{Name: "mint"},
		{Name: "burn"},
	}}

	fn, ok := iface.Function("burn")
	require.True(t, ok)
	assert.Equal(t, "burn", fn.Name)

	_, ok = iface.Function("Burn")
	assert.False(t, ok, "lookup is case-sensitive, matching Soroban symbol semantics")

	_, ok = iface.Function("missing")
	assert.False(t, ok)
}

func TestInterfaceSymbolNames(t *testing.T) {
	t.Parallel()

	iface := &model.Interface{
		Functions: []model.Function{{Name: "vote"}, {Name: "get_proposal"}},
		Types: model.Types{
			Structs:    []model.Struct{{Name: "Proposal"}},
			Unions:     []model.Union{{Name: "DataKey"}},
			Enums:      []model.Enum{{Name: "Status"}},
			ErrorEnums: []model.ErrorEnum{{Name: "Error"}},
		},
		// An event sharing a name with a function is listed once.
		Events: []model.Event{{Name: "vote"}, {Name: "Voted"}},
	}

	assert.Equal(t,
		[]string{"DataKey", "Error", "Proposal", "Status", "Voted", "get_proposal", "vote"},
		iface.SymbolNames())
	assert.Empty(t, (&model.Interface{}).SymbolNames())
}

// TestJSONShape pins the wire format, since the ABI JSON is the project's
// public contract and other tools are expected to parse it.
func TestJSONShape(t *testing.T) {
	t.Parallel()

	iface := &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions: []model.Function{{
			Name:    "hello",
			Inputs:  []model.Param{{Name: "to", Type: model.Scalar(model.KindSymbol)}},
			Outputs: []model.Type{{Kind: model.KindVec, Element: ptr(model.Scalar(model.KindSymbol)), Display: "Vec<Symbol>"}},
		}},
		Types:  model.Types{Structs: []model.Struct{}, Unions: []model.Union{}, Enums: []model.Enum{}, ErrorEnums: []model.ErrorEnum{}},
		Events: []model.Event{},
	}

	raw, err := json.Marshal(iface)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Equal(t, "1", got["abi_version"])
	assert.NotContains(t, got, "meta", "absent metadata must be omitted, not rendered as null")
	assert.NotContains(t, got, "env_meta", "absent environment metadata must be omitted, not rendered as null")

	fn := got["functions"].([]any)[0].(map[string]any)
	assert.Equal(t, "hello", fn["name"])
	assert.NotContains(t, fn, "doc", "an empty doc must be omitted")

	out := fn["outputs"].([]any)[0].(map[string]any)
	assert.Equal(t, "vec", out["kind"])
	assert.Equal(t, "Vec<Symbol>", out["display"])
	assert.Equal(t, "symbol", out["element"].(map[string]any)["kind"])
	assert.NotContains(t, out, "n", "a zero BytesN length must not appear on a non-BytesN type")

	// Empty collections must marshal as [] rather than null, so consumers
	// can iterate without a nil check.
	assert.Equal(t, []any{}, got["events"])
	assert.Equal(t, []any{}, got["types"].(map[string]any)["structs"])
}

// TestEnvMeta checks the environment metadata's own shape: the interface
// version is written the way a version number is, and the section's two
// numbers are kept apart.
func TestEnvMeta(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		protocol, preRelease uint32
		wantVersion          string
	}{
		{"a released protocol", 25, 0, "25"},
		{"a pre-release", 25, 3, "25.3"},
		{"the first protocol", 20, 0, "20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			meta := model.NewEnvMeta(tt.protocol, tt.preRelease)
			assert.Equal(t, tt.wantVersion, meta.InterfaceVersion)
			assert.Equal(t, tt.protocol, meta.Protocol)
			assert.Equal(t, tt.preRelease, meta.PreRelease)
		})
	}
}

// TestEnvMetaJSONShape pins the JSON the environment metadata is written
// as, since the ABI is the project's public contract.
func TestEnvMetaJSONShape(t *testing.T) {
	t.Parallel()

	meta := model.NewEnvMeta(25, 0)
	raw, err := json.Marshal(meta)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "25", got["interface_version"])
	assert.Equal(t, float64(25), got["protocol"])
	assert.NotContains(t, got, "pre_release", "a zero pre-release number must be omitted")
}
