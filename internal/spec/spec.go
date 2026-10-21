// Package spec decodes a Soroban contract's interface out of its WASM.
//
// The interface lives in the "contractspecv0" custom section as a bare
// stream of XDR-encoded SCSpecEntry values — no header, no delimiter and no
// length prefix between entries (SEP-48). This package reads that stream and
// converts it into the SDK-independent model.Interface, so nothing above
// this layer needs to know about XDR.
package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/wasm"
)

// ErrNoSpecSection reports that a module carried no "contractspecv0" custom
// section. That normally means the WASM is not a Soroban contract, or was
// built with the spec section stripped.
var ErrNoSpecSection = errors.New("spec: contract has no contractspecv0 section")

// ErrLimitExceeded reports that a module or interface is larger or deeper
// than SoroVault is willing to decode. It is a property of the input, not a
// server fault, so the API reports it as 422 instead of 500.
var ErrLimitExceeded = errors.New("spec: interface exceeds decode limits")

const (
	// MaxSpecSectionSize bounds the contractspecv0 section we will decode.
	// The network's 128 KiB module cap already bounds it today; 1 MiB leaves
	// headroom for that cap to grow while ensuring a hostile section cannot
	// be read, copied and decoded without limit.
	MaxSpecSectionSize = 1 << 20

	// MaxSpecEntries bounds how many SCSpecEntry values a section may hold.
	// A real contract has tens to low hundreds; 10,000 is far above any
	// legitimate interface and stops a section made of millions of tiny
	// entries from filling memory with decoded values.
	MaxSpecEntries = 10_000
)

// Decoder turns a contract's WASM into its decoded interface.
//
// It exists so callers can be tested against a stub, and so an alternative
// source of interfaces (a SEP-48 sidecar file, say, or a future spec
// version) can be dropped in without touching the layers above.
type Decoder interface {
	Decode(wasmModule []byte) (*model.Interface, error)
}

// XDRDecoder is the Decoder that reads the on-chain WASM custom sections.
// Its zero value is ready to use.
type XDRDecoder struct{}

// compile-time check that XDRDecoder satisfies Decoder.
var _ Decoder = XDRDecoder{}

// Decode extracts and converts a contract's interface from its WASM module.
//
// A missing metadata section is not an error — plenty of contracts have
// none — but a missing spec section is, and is reported as ErrNoSpecSection.
func (XDRDecoder) Decode(wasmModule []byte) (*model.Interface, error) {
	section, err := wasm.CustomSection(wasmModule, wasm.SectionContractSpec)
	if err != nil {
		if errors.Is(err, wasm.ErrSectionNotFound) {
			return nil, ErrNoSpecSection
		}
		if errors.Is(err, wasm.ErrModuleTooLarge) {
			// Keep both sentinels matchable: this is a decode limit, and it
			// came from the module reader.
			return nil, fmt.Errorf("%w: %w", ErrLimitExceeded, err)
		}
		return nil, err
	}

	entries, err := ReadEntries(section)
	if err != nil {
		return nil, err
	}

	iface, err := FromEntries(entries)
	if err != nil {
		return nil, err
	}

	meta, err := decodeMeta(wasmModule)
	if err != nil {
		return nil, err
	}
	iface.Meta = meta

	envMeta, err := decodeEnvMeta(wasmModule)
	if err != nil {
		return nil, err
	}
	iface.EnvMeta = envMeta

	return iface, nil
}

// ReadEntries decodes the concatenated stream of SCSpecEntry values that
// makes up a "contractspecv0" section body.
func ReadEntries(section []byte) ([]xdr.ScSpecEntry, error) {
	if len(section) > MaxSpecSectionSize {
		return nil, fmt.Errorf("%w: spec section is %d bytes, limit is %d",
			ErrLimitExceeded, len(section), MaxSpecSectionSize)
	}

	var entries []xdr.ScSpecEntry

	r := bytes.NewReader(section)
	for r.Len() > 0 {
		if len(entries) == MaxSpecEntries {
			return nil, fmt.Errorf("%w: more than %d spec entries", ErrLimitExceeded, MaxSpecEntries)
		}

		var entry xdr.ScSpecEntry
		if _, err := xdr.Unmarshal(r, &entry); err != nil {
			return nil, fmt.Errorf("spec: decoding entry %d: %w", len(entries), err)
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// FromEntries converts decoded XDR entries into the public interface model.
func FromEntries(entries []xdr.ScSpecEntry) (*model.Interface, error) {
	if len(entries) > MaxSpecEntries {
		return nil, fmt.Errorf("%w: %d spec entries, limit is %d",
			ErrLimitExceeded, len(entries), MaxSpecEntries)
	}

	iface := &model.Interface{
		ABIVersion: model.ABIVersion,
		Functions:  []model.Function{},
		Events:     []model.Event{},
		Types: model.Types{
			Structs:    []model.Struct{},
			Unions:     []model.Union{},
			Enums:      []model.Enum{},
			ErrorEnums: []model.ErrorEnum{},
		},
	}

	for i, entry := range entries {
		if err := appendEntry(iface, entry); err != nil {
			return nil, fmt.Errorf("spec: entry %d: %w", i, err)
		}
	}

	return iface, nil
}

// appendEntry converts one spec entry and files it under the right part of
// the interface.
func appendEntry(iface *model.Interface, entry xdr.ScSpecEntry) error {
	switch entry.Kind {
	case xdr.ScSpecEntryKindScSpecEntryFunctionV0:
		fn, err := convertFunction(entry.FunctionV0)
		if err != nil {
			return err
		}
		iface.Functions = append(iface.Functions, fn)

	case xdr.ScSpecEntryKindScSpecEntryUdtStructV0:
		s, err := convertStruct(entry.UdtStructV0)
		if err != nil {
			return err
		}
		iface.Types.Structs = append(iface.Types.Structs, s)

	case xdr.ScSpecEntryKindScSpecEntryUdtUnionV0:
		u, err := convertUnion(entry.UdtUnionV0)
		if err != nil {
			return err
		}
		iface.Types.Unions = append(iface.Types.Unions, u)

	case xdr.ScSpecEntryKindScSpecEntryUdtEnumV0:
		e, err := convertEnum(entry.UdtEnumV0)
		if err != nil {
			return err
		}
		iface.Types.Enums = append(iface.Types.Enums, e)

	case xdr.ScSpecEntryKindScSpecEntryUdtErrorEnumV0:
		e, err := convertErrorEnum(entry.UdtErrorEnumV0)
		if err != nil {
			return err
		}
		iface.Types.ErrorEnums = append(iface.Types.ErrorEnums, e)

	case xdr.ScSpecEntryKindScSpecEntryEventV0:
		ev, err := convertEvent(entry.EventV0)
		if err != nil {
			return err
		}
		iface.Events = append(iface.Events, ev)

	default:
		// A newer SDK may emit entry kinds this build predates. Skipping
		// keeps an otherwise-usable interface available rather than
		// failing the whole contract.
		return nil
	}

	return nil
}

func convertFunction(fn *xdr.ScSpecFunctionV0) (model.Function, error) {
	if fn == nil {
		return model.Function{}, errors.New("function entry has no body")
	}

	out := model.Function{
		Name:    string(fn.Name),
		Doc:     fn.Doc,
		Inputs:  make([]model.Param, 0, len(fn.Inputs)),
		Outputs: make([]model.Type, 0, len(fn.Outputs)),
	}

	for i := range fn.Inputs {
		in := fn.Inputs[i]
		t, err := ConvertType(in.Type)
		if err != nil {
			return model.Function{}, fmt.Errorf("input %q: %w", in.Name, err)
		}
		out.Inputs = append(out.Inputs, model.Param{Name: in.Name, Doc: in.Doc, Type: t})
	}

	for i := range fn.Outputs {
		t, err := ConvertType(fn.Outputs[i])
		if err != nil {
			return model.Function{}, fmt.Errorf("output %d: %w", i, err)
		}
		out.Outputs = append(out.Outputs, t)
	}

	return out, nil
}

func convertStruct(s *xdr.ScSpecUdtStructV0) (model.Struct, error) {
	if s == nil {
		return model.Struct{}, errors.New("struct entry has no body")
	}

	out := model.Struct{
		Name:   s.Name,
		Doc:    s.Doc,
		Lib:    s.Lib,
		Fields: make([]model.StructField, 0, len(s.Fields)),
	}

	for i := range s.Fields {
		f := s.Fields[i]
		t, err := ConvertType(f.Type)
		if err != nil {
			return model.Struct{}, fmt.Errorf("field %q: %w", f.Name, err)
		}
		out.Fields = append(out.Fields, model.StructField{Name: f.Name, Doc: f.Doc, Type: t})
	}

	out.IsTuple = isTupleStruct(out.Fields)
	return out, nil
}

// isTupleStruct reports whether a struct's fields are the positional indices
// "0", "1", ... that Soroban emits for a Rust tuple struct. An empty struct
// is a unit struct, not a tuple.
func isTupleStruct(fields []model.StructField) bool {
	if len(fields) == 0 {
		return false
	}
	for i, f := range fields {
		if f.Name != strconv.Itoa(i) {
			return false
		}
	}
	return true
}

func convertUnion(u *xdr.ScSpecUdtUnionV0) (model.Union, error) {
	if u == nil {
		return model.Union{}, errors.New("union entry has no body")
	}

	out := model.Union{
		Name:  u.Name,
		Doc:   u.Doc,
		Lib:   u.Lib,
		Cases: make([]model.UnionCase, 0, len(u.Cases)),
	}

	for i := range u.Cases {
		c := u.Cases[i]
		switch c.Kind {
		case xdr.ScSpecUdtUnionCaseV0KindScSpecUdtUnionCaseVoidV0:
			if c.VoidCase == nil {
				return model.Union{}, fmt.Errorf("case %d: void case has no body", i)
			}
			out.Cases = append(out.Cases, model.UnionCase{
				Name:   c.VoidCase.Name,
				Doc:    c.VoidCase.Doc,
				Values: []model.Type{},
			})

		case xdr.ScSpecUdtUnionCaseV0KindScSpecUdtUnionCaseTupleV0:
			if c.TupleCase == nil {
				return model.Union{}, fmt.Errorf("case %d: tuple case has no body", i)
			}
			values := make([]model.Type, 0, len(c.TupleCase.Type))
			for j := range c.TupleCase.Type {
				t, err := ConvertType(c.TupleCase.Type[j])
				if err != nil {
					return model.Union{}, fmt.Errorf("case %q value %d: %w", c.TupleCase.Name, j, err)
				}
				values = append(values, t)
			}
			out.Cases = append(out.Cases, model.UnionCase{
				Name:   c.TupleCase.Name,
				Doc:    c.TupleCase.Doc,
				Values: values,
			})

		default:
			return model.Union{}, fmt.Errorf("case %d: unknown union case kind %d", i, c.Kind)
		}
	}

	return out, nil
}

func convertEnum(e *xdr.ScSpecUdtEnumV0) (model.Enum, error) {
	if e == nil {
		return model.Enum{}, errors.New("enum entry has no body")
	}

	out := model.Enum{
		Name:  e.Name,
		Doc:   e.Doc,
		Lib:   e.Lib,
		Cases: make([]model.EnumCase, 0, len(e.Cases)),
	}
	for i := range e.Cases {
		c := e.Cases[i]
		out.Cases = append(out.Cases, model.EnumCase{Name: c.Name, Doc: c.Doc, Value: uint32(c.Value)})
	}
	return out, nil
}

func convertErrorEnum(e *xdr.ScSpecUdtErrorEnumV0) (model.ErrorEnum, error) {
	if e == nil {
		return model.ErrorEnum{}, errors.New("error enum entry has no body")
	}

	out := model.ErrorEnum{
		Name:  e.Name,
		Doc:   e.Doc,
		Lib:   e.Lib,
		Cases: make([]model.EnumCase, 0, len(e.Cases)),
	}
	for i := range e.Cases {
		c := e.Cases[i]
		out.Cases = append(out.Cases, model.EnumCase{Name: c.Name, Doc: c.Doc, Value: uint32(c.Value)})
	}
	return out, nil
}

func convertEvent(e *xdr.ScSpecEventV0) (model.Event, error) {
	if e == nil {
		return model.Event{}, errors.New("event entry has no body")
	}

	out := model.Event{
		Name:       string(e.Name),
		Doc:        e.Doc,
		Lib:        e.Lib,
		Params:     make([]model.EventParam, 0, len(e.Params)),
		DataFormat: dataFormatName(e.DataFormat),
	}

	for i := range e.PrefixTopics {
		out.Prefix = append(out.Prefix, string(e.PrefixTopics[i]))
	}

	for i := range e.Params {
		p := e.Params[i]
		t, err := ConvertType(p.Type)
		if err != nil {
			return model.Event{}, fmt.Errorf("param %q: %w", p.Name, err)
		}
		out.Params = append(out.Params, model.EventParam{
			Name:     p.Name,
			Doc:      p.Doc,
			Type:     t,
			Location: paramLocationName(p.Location),
		})
	}

	return out, nil
}

func dataFormatName(f xdr.ScSpecEventDataFormat) string {
	switch f {
	case xdr.ScSpecEventDataFormatScSpecEventDataFormatSingleValue:
		return "single-value"
	case xdr.ScSpecEventDataFormatScSpecEventDataFormatVec:
		return "vec"
	case xdr.ScSpecEventDataFormatScSpecEventDataFormatMap:
		return "map"
	default:
		return "unknown"
	}
}

func paramLocationName(l xdr.ScSpecEventParamLocationV0) string {
	switch l {
	case xdr.ScSpecEventParamLocationV0ScSpecEventParamLocationData:
		return "data"
	case xdr.ScSpecEventParamLocationV0ScSpecEventParamLocationTopicList:
		return "topic-list"
	default:
		return "unknown"
	}
}

// decodeMeta reads the optional "contractmetav0" section into a flat map.
func decodeMeta(wasmModule []byte) (map[string]string, error) {
	section, err := wasm.CustomSection(wasmModule, wasm.SectionContractMeta)
	if err != nil {
		if errors.Is(err, wasm.ErrSectionNotFound) {
			return nil, nil
		}
		return nil, err
	}

	meta := make(map[string]string)
	r := bytes.NewReader(section)
	for r.Len() > 0 {
		var entry xdr.ScMetaEntry
		if _, err := xdr.Unmarshal(r, &entry); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("spec: decoding meta entry: %w", err)
		}
		if entry.Kind == xdr.ScMetaKindScMetaV0 && entry.V0 != nil {
			meta[entry.V0.Key] = entry.V0.Val
		}
	}

	if len(meta) == 0 {
		return nil, nil
	}
	return meta, nil
}

// decodeEnvMeta reads the optional "contractenvmetav0" section, which
// records the host environment the contract was built against.
//
// The section holds a stream of SCEnvMetaEntry values. The only kind there
// is today is the interface version, which the host checks before it runs
// the contract: a contract built for the same or an older protocol version
// runs, and a contract with a different pre-release number does not.
//
// The interface version used to be one 64-bit number that packed the
// protocol version into its high half and the pre-release number into its
// low half; the XDR now spells the two out as separate 32-bit fields. The
// bytes mean the same thing either way — XDR is big-endian, so the high
// half comes first — and a section written by an older toolchain still
// decodes.
func decodeEnvMeta(wasmModule []byte) (*model.EnvMeta, error) {
	section, err := wasm.CustomSection(wasmModule, wasm.SectionContractEnvMeta)
	if err != nil {
		if errors.Is(err, wasm.ErrSectionNotFound) {
			return nil, nil
		}
		return nil, err
	}

	var out *model.EnvMeta
	r := bytes.NewReader(section)
	for r.Len() > 0 {
		var entry xdr.ScEnvMetaEntry
		if _, err := xdr.Unmarshal(r, &entry); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("spec: decoding env meta entry: %w", err)
		}
		if entry.Kind != xdr.ScEnvMetaKindScEnvMetaKindInterfaceVersion || entry.InterfaceVersion == nil {
			// A kind this build predates. Skipping it keeps the rest of the
			// metadata available.
			continue
		}
		meta := model.NewEnvMeta(
			uint32(entry.InterfaceVersion.Protocol),
			uint32(entry.InterfaceVersion.PreRelease),
		)
		out = &meta
	}

	return out, nil
}
