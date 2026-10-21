package codegen

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/soroworks/sorovault/internal/model"
)

// jsonSchemaDialect is the JSON Schema dialect the generated schemas
// declare, and the one the OpenAPI document's schemas are written in.
const jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// JSONSchema renders a JSON Schema (2020-12) for one function's arguments.
//
// The schema describes the arguments as a JSON object whose keys are the
// parameter names the interface declares, so a wallet or a dApp can render
// a form from it and validate what the user typed before encoding it.
//
// The mapping follows the conventions of the TypeScript generator, so a
// value that satisfies the schema is a value the generated client accepts:
//
//   - An integer that fits in 53 bits — u32, i32 — is a JSON number. One
//     that does not — u64 and wider, and the u64-backed timepoint and
//     duration — is a decimal string, because a JSON number cannot hold it
//     exactly.
//   - An address, a symbol, a byte string and a fixed-length byte string
//     are strings with a pattern that says what they may contain.
//   - An Option is nullable: it is either null or the type it wraps.
//   - A user-defined type becomes an entry in $defs, referenced by name.
//   - A collection is an array, and a map is an object whose values are
//     the map's value type.
//
// The schema is written for the interface it is generated from, so the
// same function can have a different schema in a later interface version.
func JSONSchema(iface *model.Interface, fn model.Function, src Source) ([]byte, error) {
	if iface == nil {
		return nil, fmt.Errorf("codegen: no interface to generate a schema from")
	}

	g := &schemaGen{iface: iface, defs: map[string]any{}}

	properties := map[string]any{}
	required := make([]string, 0, len(fn.Inputs))
	for _, in := range fn.Inputs {
		properties[in.Name] = g.property(in)
		required = append(required, in.Name)
	}

	schema := map[string]any{
		"$schema":              jsonSchemaDialect,
		"$id":                  schemaID(src, fn.Name),
		"title":                fn.Name + " arguments",
		"description":          schemaDescription(fn, src),
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	if len(g.defs) > 0 {
		schema["$defs"] = g.defs
	}

	body, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("codegen: encoding the schema for %s: %w", fn.Name, err)
	}
	return append(body, '\n'), nil
}

// schemaID is the schema's canonical identifier.
//
// It names the contract, the network, the interface version and the
// function, so two schemas for two versions of the same contract are two
// different schemas. A URN is used rather than a URL: the registry does
// not know the address it is served under, and the identifier has to be
// the same wherever the schema is generated.
func schemaID(src Source, fnName string) string {
	return "urn:sorovault:contract:" + src.ContractID + ":" + src.Network +
		":" + src.WasmHash + ":" + fnName
}

// schemaDescription describes the schema in a sentence, or uses the
// function's own documentation when it has one.
func schemaDescription(fn model.Function, src Source) string {
	if doc := strings.TrimSpace(fn.Doc); doc != "" {
		return doc
	}
	return "Arguments of " + fn.Name + " on contract " + src.ContractID +
		" (" + src.Network + ")."
}

// schemaGen builds the schemas for one function's arguments, collecting
// the user-defined types they refer to on the way.
type schemaGen struct {
	iface *model.Interface
	defs  map[string]any
}

// property is the schema for one named argument. A documented argument
// carries its documentation as the schema's description.
func (g *schemaGen) property(in model.Param) map[string]any {
	s := g.schema(in.Type)
	doc := strings.TrimSpace(in.Doc)
	if doc == "" {
		return s
	}
	if _, isRef := s["$ref"]; isRef {
		// A $ref with siblings is only legal from 2020-12 on, and not
		// every validator reads it that way, so the reference is wrapped
		// and the description sits beside it.
		return map[string]any{"description": doc, "allOf": []any{s}}
	}
	s["description"] = doc
	return s
}

// child is the schema of a nested type, tolerating a missing one.
func (g *schemaGen) child(t *model.Type) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	return g.schema(*t)
}

// schema maps an interface type onto a JSON Schema.
func (g *schemaGen) schema(t model.Type) map[string]any {
	switch t.Kind {
	case model.KindVal:
		// A Val is anything the host accepts.
		return map[string]any{}

	case model.KindBool:
		return map[string]any{"type": "boolean"}

	case model.KindVoid:
		return map[string]any{"type": "null"}

	case model.KindError:
		// An error is reported as its numeric code.
		return map[string]any{"type": "integer", "minimum": 0, "maximum": 4294967295}

	case model.KindU32:
		return map[string]any{"type": "integer", "minimum": 0, "maximum": 4294967295}

	case model.KindI32:
		return map[string]any{"type": "integer", "minimum": -2147483648, "maximum": 2147483647}

	case model.KindU64, model.KindU128, model.KindU256, model.KindTimepoint:
		// Wider than 53 bits, so a decimal string rather than a number.
		return map[string]any{"type": "string", "pattern": "^[0-9]+$"}

	case model.KindI64, model.KindI128, model.KindI256:
		return map[string]any{"type": "string", "pattern": "^-?[0-9]+$"}

	case model.KindDuration:
		return map[string]any{"type": "string", "pattern": "^[0-9]+$"}

	case model.KindBytes:
		// Hexadecimal, as the Stellar CLI and SoroProbe write bytes.
		return map[string]any{"type": "string", "pattern": "^(?:[0-9a-fA-F]{2})*$"}

	case model.KindBytesN:
		return map[string]any{
			"type":    "string",
			"pattern": fmt.Sprintf("^[0-9a-fA-F]{%d}$", 2*t.N),
		}

	case model.KindString:
		return map[string]any{"type": "string"}

	case model.KindSymbol:
		// Soroban's symbols are at most 32 characters of letters,
		// digits and underscores.
		return map[string]any{"type": "string", "maxLength": 32, "pattern": "^[a-zA-Z0-9_]*$"}

	case model.KindAddress:
		// A contract (C) or an account (G) address.
		return map[string]any{"type": "string", "pattern": "^(?:C|G)[A-Z2-7]{55}$"}

	case model.KindMuxedAddress:
		// A muxed (M) address is an account address with a multiplexed
		// id, so all three spellings are allowed.
		return map[string]any{"type": "string", "pattern": "^(?:C|G|M)[A-Z2-7]{55}$"}

	case model.KindOption:
		// An Option is either null or the type it wraps.
		return map[string]any{
			"oneOf": []any{map[string]any{"type": "null"}, g.child(t.Inner)},
		}

	case model.KindVec:
		return map[string]any{"type": "array", "items": g.child(t.Element)}

	case model.KindMap:
		// A map's keys are strings in JSON; the value type is what the
		// map holds.
		return map[string]any{"type": "object", "additionalProperties": g.child(t.Value)}

	case model.KindTuple:
		items := make([]any, 0, len(t.Elements))
		for _, element := range t.Elements {
			items = append(items, g.schema(element))
		}
		return map[string]any{
			"type":        "array",
			"prefixItems": items,
			"minItems":    len(items),
			"maxItems":    len(items),
		}

	case model.KindResult:
		// The SDK's Result is either a value or an error, and so is
		// this.
		return map[string]any{
			"oneOf": []any{g.child(t.Ok), g.child(t.Error)},
		}

	case model.KindUDT:
		g.definition(t.Name)
		return map[string]any{"$ref": "#/$defs/" + t.Name}

	default:
		return map[string]any{}
	}
}

// definition adds a user-defined type to $defs, once.
func (g *schemaGen) definition(name string) {
	if _, done := g.defs[name]; done {
		return
	}
	// The placeholder stands in while the definition is built, so a type
	// that refers to itself does not loop.
	g.defs[name] = map[string]any{}
	g.defs[name] = g.buildDefinition(name)
}

// buildDefinition is the schema of one user-defined type.
func (g *schemaGen) buildDefinition(name string) map[string]any {
	for _, s := range g.iface.Types.Structs {
		if s.Name != name {
			continue
		}
		return g.structDefinition(s)
	}
	for _, u := range g.iface.Types.Unions {
		if u.Name != name {
			continue
		}
		return g.unionDefinition(u)
	}
	for _, e := range g.iface.Types.Enums {
		if e.Name != name {
			continue
		}
		return enumDefinition(e.Cases)
	}
	for _, e := range g.iface.Types.ErrorEnums {
		if e.Name != name {
			continue
		}
		// An error enum used as a value is its numeric code, the way the
		// TypeScript generator writes it.
		return enumDefinition(e.Cases)
	}
	// A type the interface names but does not define: anything goes.
	return map[string]any{}
}

// structDefinition is the schema of a struct: an object with a property
// per field, or an array when the struct is a tuple struct.
func (g *schemaGen) structDefinition(s model.Struct) map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		properties[f.Name] = g.schema(f.Type)
		required = append(required, f.Name)
	}

	if s.IsTuple {
		// A tuple struct is encoded as a struct whose fields are named
		// "0", "1", and so on; as an array it is positional.
		items := make([]any, 0, len(s.Fields))
		for _, f := range s.Fields {
			items = append(items, g.schema(f.Type))
		}
		return map[string]any{
			"type":        "array",
			"prefixItems": items,
			"minItems":    len(items),
			"maxItems":    len(items),
		}
	}

	definition := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		definition["required"] = required
	}
	return definition
}

// unionDefinition is the schema of a union: the SDK's tagged object, one
// alternative per case.
func (g *schemaGen) unionDefinition(u model.Union) map[string]any {
	cases := make([]any, 0, len(u.Cases))
	for _, c := range u.Cases {
		values := map[string]any{"type": "null"}
		if len(c.Values) > 0 {
			items := make([]any, 0, len(c.Values))
			for _, v := range c.Values {
				items = append(items, g.schema(v))
			}
			values = map[string]any{
				"type":        "array",
				"prefixItems": items,
				"minItems":    len(items),
				"maxItems":    len(items),
			}
		}
		cases = append(cases, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tag":    map[string]any{"const": c.Name},
				"values": values,
			},
			"required":             []any{"tag", "values"},
			"additionalProperties": false,
		})
	}
	return map[string]any{"oneOf": cases}
}

// enumDefinition is the schema of an enumeration: the numbers it can
// take.
func enumDefinition(cases []model.EnumCase) map[string]any {
	values := make([]any, 0, len(cases))
	for _, c := range cases {
		values = append(values, c.Value)
	}
	return map[string]any{"type": "integer", "enum": values}
}
