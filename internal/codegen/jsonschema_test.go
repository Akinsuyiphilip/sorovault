package codegen_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/codegen"
	"github.com/soroworks/sorovault/internal/model"
)

// zkvoteInterface reads the decoded ABI of the real testnet contract the
// spec package pins, so the schemas are generated from a genuine
// interface rather than a hand-written one.
func zkvoteInterface(t *testing.T) *model.Interface {
	t.Helper()
	raw, err := os.ReadFile("../spec/testdata/zkvote.golden.json")
	require.NoError(t, err)
	var iface model.Interface
	require.NoError(t, json.Unmarshal(raw, &iface))
	return &iface
}

// compileSchema compiles a generated schema with a JSON Schema validator,
// so the test proves the document is a valid 2020-12 schema and not merely
// valid JSON.
func compileSchema(t *testing.T, schema []byte) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	require.NoError(t, err)

	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("schema.json", doc))
	compiled, err := c.Compile("schema.json")
	require.NoError(t, err)
	return compiled
}

// validate checks an instance against a compiled schema. The instance is
// read by the validator's own decoder, so numbers keep their exact value.
func validate(t *testing.T, sch *jsonschema.Schema, instance string) error {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(instance))
	require.NoError(t, err)
	return sch.Validate(value)
}

// TestGoldenZKVoteSchemas generates a JSON Schema for every function of the
// zkvote contract, compares each with a checked-in file, and compiles it
// with a JSON Schema validator.
func TestGoldenZKVoteSchemas(t *testing.T) {
	t.Parallel()

	iface := zkvoteInterface(t)
	dir := filepath.Join("testdata", "zkvote.schemas")
	if *update {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	for _, fn := range iface.Functions {
		t.Run(fn.Name, func(t *testing.T) {
			t.Parallel()

			got, err := codegen.JSONSchema(iface, fn, src)
			require.NoError(t, err)

			golden := filepath.Join(dir, fn.Name+".json")
			if *update {
				require.NoError(t, os.WriteFile(golden, got, 0o644))
				t.Logf("wrote %s", golden)
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err, "golden schema missing; regenerate with -update")
			assert.Equal(t, string(want), string(got),
				"generated schema changed; if intended, rerun with -update")

			compileSchema(t, got)
		})
	}
}

// TestJSONSchemaValidatesArguments checks, with a JSON Schema validator,
// that the generated schemas accept what the contract accepts and reject
// what it does not.
func TestJSONSchemaValidatesArguments(t *testing.T) {
	t.Parallel()

	iface := &model.Interface{
		Types: model.Types{
			Structs: []model.Struct{{Name: "VoteParams", Fields: []model.StructField{
				{Name: "scope", Type: model.Scalar(model.KindU64)},
				{Name: "quorum", Type: model.Scalar(model.KindU32)},
			}}},
		},
		Functions: []model.Function{{Name: "cast", Inputs: []model.Param{
			{Name: "voter", Type: model.Scalar(model.KindAddress)},
			{Name: "index", Type: model.Scalar(model.KindU32)},
			{Name: "amount", Type: model.Scalar(model.KindI128)},
			{Name: "proof", Type: model.Type{Kind: model.KindBytesN, N: 32}},
			{Name: "params", Type: model.Type{
				Kind:  model.KindOption,
				Inner: ptr(model.Type{Kind: model.KindUDT, Name: "VoteParams"}),
			}},
		}}},
	}

	fn, ok := iface.Function("cast")
	require.True(t, ok)

	schema, err := codegen.JSONSchema(iface, fn, src)
	require.NoError(t, err)
	sch := compileSchema(t, schema)

	const address = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
	const proof = "0000000000000000000000000000000000000000000000000000000000000000"

	valid := `{
		"voter": "` + address + `",
		"index": 3,
		"amount": "-170141183460469231731687303715884105728",
		"proof": "` + proof + `",
		"params": {"scope": "18446744073709551615", "quorum": 2}
	}`
	require.NoError(t, validate(t, sch, valid), "a valid argument set must be accepted")

	// An Option is either null or the type it wraps.
	withNull := `{
		"voter": "` + address + `",
		"index": 3,
		"amount": "1",
		"proof": "` + proof + `",
		"params": null
	}`
	require.NoError(t, validate(t, sch, withNull), "a null option must be accepted")

	invalid := map[string]string{
		"a lowercase address":              `{"voter": "gaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaawhf"}`,
		"an address of the wrong length":   `{"voter": "GAAA"}`,
		"a u32 that is a string":           `{"index": "3"}`,
		"a u32 that is negative":           `{"index": -1}`,
		"an i128 that is a number":         `{"amount": 1}`,
		"an i128 that is not a number":     `{"amount": "one"}`,
		"a proof that is not hexadecimal":  `{"proof": "zz00000000000000000000000000000000000000000000000000000000000000"}`,
		"a proof of the wrong length":      `{"proof": "0000"}`,
		"a missing argument":               `{}`,
		"an argument that is not declared": `{"extra": 1}`,
	}
	for name, instance := range invalid {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, validate(t, sch, instance))
		})
	}
}

// TestJSONSchemaMapsTypesToStrings checks the conventions the schema
// shares with the TypeScript generator: an integer too wide for a JSON
// number is a string, and a fixed-length byte string is hexadecimal.
func TestJSONSchemaMapsTypesToStrings(t *testing.T) {
	t.Parallel()

	iface := &model.Interface{
		Functions: []model.Function{{Name: "wide", Inputs: []model.Param{
			{Name: "small", Type: model.Scalar(model.KindU32)},
			{Name: "large", Type: model.Scalar(model.KindU64)},
			{Name: "signed", Type: model.Scalar(model.KindI128)},
			{Name: "fixed", Type: model.Type{Kind: model.KindBytesN, N: 4}},
		}}},
	}

	fn, ok := iface.Function("wide")
	require.True(t, ok)

	schema, err := codegen.JSONSchema(iface, fn, src)
	require.NoError(t, err)

	var doc struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(schema, &doc))

	assert.Equal(t, "integer", doc.Properties["small"].Type, "a u32 fits in a JSON number")
	assert.Equal(t, "string", doc.Properties["large"].Type, "a u64 does not fit in a JSON number")
	assert.Equal(t, "string", doc.Properties["signed"].Type, "an i128 does not fit in a JSON number")
	assert.Equal(t, "^[0-9]+$", doc.Properties["large"].Pattern)
	assert.Equal(t, "^-?[0-9]+$", doc.Properties["signed"].Pattern)
	assert.Equal(t, "^[0-9a-fA-F]{8}$", doc.Properties["fixed"].Pattern, "four bytes are eight hex digits")
}

// TestJSONSchemaRejectsAnEmptyInterface checks the one case where there is
// nothing to describe.
func TestJSONSchemaRejectsAnEmptyInterface(t *testing.T) {
	t.Parallel()

	_, err := codegen.JSONSchema(nil, model.Function{Name: "f"}, src)
	assert.ErrorContains(t, err, "no interface")
}
