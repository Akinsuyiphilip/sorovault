package codegen_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/codegen"
	"github.com/soroworks/sorovault/internal/model"
)

var update = flag.Bool("update", false, "rewrite the golden TypeScript client")

func ptr(t model.Type) *model.Type { return &t }

var src = codegen.Source{
	ContractID: "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN",
	Network:    "testnet",
	WasmHash:   "abc123",
}

// TestGoldenZKVote generates a client for the real testnet contract whose
// decoded ABI the spec package pins, and compares it with a checked-in file.
// CI type-checks that file against @stellar/stellar-sdk, so the golden is
// known to compile, not just to be stable.
func TestGoldenZKVote(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../spec/testdata/zkvote.golden.json")
	require.NoError(t, err)
	var iface model.Interface
	require.NoError(t, json.Unmarshal(raw, &iface))

	got, err := codegen.TypeScript(&iface, src)
	require.NoError(t, err)

	golden := filepath.Join("testdata", "zkvote.client.ts")
	if *update {
		require.NoError(t, os.WriteFile(golden, []byte(got), 0o644))
		t.Logf("wrote %s", golden)
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "golden file missing; regenerate with -update")
	assert.Equal(t, string(want), got, "generated client changed; if intended, rerun with -update")
}

func TestTypeMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   model.Type
		want string
	}{
		{model.Scalar(model.KindU32), "sdk.u32"},
		{model.Scalar(model.KindI128), "sdk.i128"},
		{model.Scalar(model.KindTimepoint), "sdk.Timepoint"},
		{model.Scalar(model.KindBool), "boolean"},
		{model.Scalar(model.KindAddress), "string"},
		{model.Scalar(model.KindSymbol), "string"},
		{model.Scalar(model.KindBytes), "Buffer"},
		{model.Type{Kind: model.KindBytesN, N: 32}, "Buffer"},
		{model.Type{Kind: model.KindOption, Inner: ptr(model.Scalar(model.KindU64))}, "sdk.Option<sdk.u64>"},
		{model.Type{Kind: model.KindVec, Element: ptr(model.Scalar(model.KindString))}, "Array<string>"},
		{model.Type{Kind: model.KindMap, Key: ptr(model.Scalar(model.KindSymbol)), Value: ptr(model.Scalar(model.KindI128))}, "Map<string, sdk.i128>"},
		{model.Type{Kind: model.KindTuple, Elements: []model.Type{model.Scalar(model.KindU32), model.Scalar(model.KindBool)}}, "readonly [sdk.u32, boolean]"},
		{model.Type{Kind: model.KindResult, Ok: ptr(model.Scalar(model.KindVoid)), Error: ptr(model.Type{Kind: model.KindUDT, Name: "Error"})}, "sdk.Result<void>"},
		{model.Type{Kind: model.KindUDT, Name: "Proposal"}, "Proposal"},
		{model.Type{Kind: model.KindUDT, Name: "Map"}, "Map_"},
	}

	for _, tt := range tests {
		iface := &model.Interface{Functions: []model.Function{{Name: "f", Outputs: []model.Type{tt.in}}}}
		got, err := codegen.TypeScript(iface, src)
		require.NoError(t, err, tt.want)
		assert.Contains(t, got, "f(options?: sdk.MethodOptions): Promise<sdk.AssembledTransaction<"+tt.want+">>;", tt.want)
	}
}

func TestErrorEnumReferenceIsItsCode(t *testing.T) {
	t.Parallel()
	iface := &model.Interface{
		Types: model.Types{ErrorEnums: []model.ErrorEnum{{Name: "Error", Cases: []model.EnumCase{{Name: "NotFound", Value: 3}}}}},
		Functions: []model.Function{{Name: "last_error",
			Outputs: []model.Type{{Kind: model.KindUDT, Name: "Error"}}}},
	}
	got, err := codegen.TypeScript(iface, src)
	require.NoError(t, err)
	assert.Contains(t, got, "Promise<sdk.AssembledTransaction<sdk.u32>>")
	assert.Contains(t, got, `3: { message: "NotFound" },`)
}

func TestDeclarations(t *testing.T) {
	t.Parallel()
	iface := &model.Interface{
		Types: model.Types{
			Structs: []model.Struct{
				{Name: "Point", IsTuple: true, Fields: []model.StructField{
					{Name: "0", Type: model.Scalar(model.KindI32)}, {Name: "1", Type: model.Scalar(model.KindI32)}}},
				{Name: "Config", Doc: "Ends early? */ no", Fields: []model.StructField{
					{Name: "admin", Type: model.Scalar(model.KindAddress)},
					{Name: "fee-bps", Type: model.Scalar(model.KindU32)}}},
			},
			Unions: []model.Union{{Name: "DataKey", Cases: []model.UnionCase{
				{Name: "Admin"},
				{Name: "Balance", Values: []model.Type{model.Scalar(model.KindAddress)}}}}},
			Enums: []model.Enum{{Name: "Status", Cases: []model.EnumCase{{Name: "Active", Value: 0}, {Name: "Closed", Value: 2}}}},
		},
		Functions: []model.Function{{Name: "transfer", Inputs: []model.Param{
			{Name: "from", Type: model.Scalar(model.KindAddress)},
			{Name: "amount", Type: model.Scalar(model.KindI128)}}}},
	}

	got, err := codegen.TypeScript(iface, src)
	require.NoError(t, err)

	for _, want := range []string{
		"export type Point = readonly [sdk.i32, sdk.i32];",
		"export interface Config {\n  admin: string;\n  \"fee-bps\": sdk.u32;\n}",
		"Ends early? *\\/ no",
		"export type DataKey =\n  | { tag: \"Admin\"; values: void }\n  | { tag: \"Balance\"; values: readonly [string] };",
		"export enum Status {\n  Active = 0,\n  Closed = 2,\n}",
		"transfer(args: { from: string; amount: sdk.i128 }, options?: sdk.MethodOptions): Promise<sdk.AssembledTransaction<null>>;",
		`export const contractId = "` + src.ContractID + `";`,
	} {
		assert.Contains(t, got, want)
	}
	assert.NotContains(t, got, "*/ no", "a doc string must not close its comment")
}

func TestRejectsConflictingErrorCodes(t *testing.T) {
	t.Parallel()
	iface := &model.Interface{Types: model.Types{ErrorEnums: []model.ErrorEnum{
		{Name: "A", Cases: []model.EnumCase{{Name: "Busy", Value: 1}}},
		{Name: "B", Cases: []model.EnumCase{{Name: "Gone", Value: 1}}},
	}}}
	_, err := codegen.TypeScript(iface, src)
	assert.ErrorContains(t, err, "error code 1")
}

func TestRejectsInvalidFunctionName(t *testing.T) {
	t.Parallel()
	iface := &model.Interface{Functions: []model.Function{{Name: "not-valid"}}}
	_, err := codegen.TypeScript(iface, src)
	assert.ErrorContains(t, err, "not a valid identifier")
}

func TestOutputIsDeterministic(t *testing.T) {
	t.Parallel()
	iface := &model.Interface{Types: model.Types{Structs: []model.Struct{{Name: "Zed"}, {Name: "Alpha"}}}}
	a, err := codegen.TypeScript(iface, src)
	require.NoError(t, err)
	assert.Less(t, strings.Index(a, "interface Alpha"), strings.Index(a, "interface Zed"))
}

func TestMarkdownDocumentsInterface(t *testing.T) {
	t.Parallel()
	iface := &model.Interface{
		Functions: []model.Function{{
			Name:    "transfer",
			Doc:     "Move tokens to an address.",
			Inputs:  []model.Param{{Name: "to", Doc: "Recipient.", Type: model.Scalar(model.KindAddress)}},
			Outputs: []model.Type{model.Scalar(model.KindBool)},
		}},
		Types: model.Types{
			Structs:    []model.Struct{{Name: "Config", Doc: "Contract settings.", Fields: []model.StructField{{Name: "fee", Doc: "Fee in bps.", Type: model.Scalar(model.KindU32)}}}},
			Unions:     []model.Union{{Name: "Action", Doc: "Supported actions.", Cases: []model.UnionCase{{Name: "Pause", Doc: "Pause transfers."}}}},
			Enums:      []model.Enum{{Name: "Status", Doc: "Current status.", Cases: []model.EnumCase{{Name: "Active", Value: 1}}}},
			ErrorEnums: []model.ErrorEnum{{Name: "ContractError", Cases: []model.EnumCase{{Name: "Unauthorized", Value: 4}}}},
		},
		Events: []model.Event{{Name: "Transfer", Doc: "Emitted after a transfer.", Params: []model.EventParam{{Name: "to", Type: model.Scalar(model.KindAddress), Location: "topic-list"}}, DataFormat: "single-value"}},
	}

	got, err := codegen.Markdown(iface, src)
	require.NoError(t, err)
	for _, want := range []string{
		"# Contract `" + src.ContractID + "`",
		"WASM hash: `" + src.WasmHash + "`",
		"### `fn transfer(to: Address) -> bool`",
		"Move tokens to an address.",
		"Recipient.",
		"## Types",
		"### `struct Config`",
		"Fee in bps.",
		"### `enum Action`",
		"Pause transfers.",
		"### `enum Status`",
		"### `error ContractError`",
		"## Events",
		"### `Transfer`",
		"Emitted after a transfer.",
	} {
		assert.Contains(t, got, want)
	}
}
