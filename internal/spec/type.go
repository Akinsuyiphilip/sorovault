package spec

import (
	"errors"
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/soroworks/sorovault/internal/model"
)

// MaxTypeDepth bounds how deeply nested a type may be before conversion
// gives up. Soroban's own limit is far lower; this exists so a malformed or
// hostile spec section cannot drive unbounded recursion. It is enforced
// during conversion, on the way down, so a too-deep type is rejected before
// a model value for it is ever built.
const MaxTypeDepth = 64

// scalarKinds maps each primitive XDR spec type onto its model kind.
// Composite types are handled separately in ConvertType.
var scalarKinds = map[xdr.ScSpecType]model.Kind{
	xdr.ScSpecTypeScSpecTypeVal:          model.KindVal,
	xdr.ScSpecTypeScSpecTypeBool:         model.KindBool,
	xdr.ScSpecTypeScSpecTypeVoid:         model.KindVoid,
	xdr.ScSpecTypeScSpecTypeError:        model.KindError,
	xdr.ScSpecTypeScSpecTypeU32:          model.KindU32,
	xdr.ScSpecTypeScSpecTypeI32:          model.KindI32,
	xdr.ScSpecTypeScSpecTypeU64:          model.KindU64,
	xdr.ScSpecTypeScSpecTypeI64:          model.KindI64,
	xdr.ScSpecTypeScSpecTypeTimepoint:    model.KindTimepoint,
	xdr.ScSpecTypeScSpecTypeDuration:     model.KindDuration,
	xdr.ScSpecTypeScSpecTypeU128:         model.KindU128,
	xdr.ScSpecTypeScSpecTypeI128:         model.KindI128,
	xdr.ScSpecTypeScSpecTypeU256:         model.KindU256,
	xdr.ScSpecTypeScSpecTypeI256:         model.KindI256,
	xdr.ScSpecTypeScSpecTypeBytes:        model.KindBytes,
	xdr.ScSpecTypeScSpecTypeString:       model.KindString,
	xdr.ScSpecTypeScSpecTypeSymbol:       model.KindSymbol,
	xdr.ScSpecTypeScSpecTypeAddress:      model.KindAddress,
	xdr.ScSpecTypeScSpecTypeMuxedAddress: model.KindMuxedAddress,
}

// ConvertType turns an XDR type definition into the public model's Type,
// recursively for container types. Every returned Type has its Display
// rendering filled in.
func ConvertType(t xdr.ScSpecTypeDef) (model.Type, error) {
	return convertType(t, 0)
}

func convertType(t xdr.ScSpecTypeDef, depth int) (model.Type, error) {
	if depth > MaxTypeDepth {
		return model.Type{}, fmt.Errorf("%w: type nested deeper than %d levels", ErrLimitExceeded, MaxTypeDepth)
	}

	if kind, ok := scalarKinds[t.Type]; ok {
		return model.Scalar(kind), nil
	}

	var out model.Type

	switch t.Type {
	case xdr.ScSpecTypeScSpecTypeOption:
		if t.Option == nil {
			return model.Type{}, errors.New("option type has no body")
		}
		inner, err := convertType(t.Option.ValueType, depth+1)
		if err != nil {
			return model.Type{}, fmt.Errorf("option value: %w", err)
		}
		out = model.Type{Kind: model.KindOption, Inner: &inner}

	case xdr.ScSpecTypeScSpecTypeVec:
		if t.Vec == nil {
			return model.Type{}, errors.New("vec type has no body")
		}
		elem, err := convertType(t.Vec.ElementType, depth+1)
		if err != nil {
			return model.Type{}, fmt.Errorf("vec element: %w", err)
		}
		out = model.Type{Kind: model.KindVec, Element: &elem}

	case xdr.ScSpecTypeScSpecTypeMap:
		if t.Map == nil {
			return model.Type{}, errors.New("map type has no body")
		}
		key, err := convertType(t.Map.KeyType, depth+1)
		if err != nil {
			return model.Type{}, fmt.Errorf("map key: %w", err)
		}
		val, err := convertType(t.Map.ValueType, depth+1)
		if err != nil {
			return model.Type{}, fmt.Errorf("map value: %w", err)
		}
		out = model.Type{Kind: model.KindMap, Key: &key, Value: &val}

	case xdr.ScSpecTypeScSpecTypeResult:
		if t.Result == nil {
			return model.Type{}, errors.New("result type has no body")
		}
		ok, err := convertType(t.Result.OkType, depth+1)
		if err != nil {
			return model.Type{}, fmt.Errorf("result ok: %w", err)
		}
		bad, err := convertType(t.Result.ErrorType, depth+1)
		if err != nil {
			return model.Type{}, fmt.Errorf("result error: %w", err)
		}
		out = model.Type{Kind: model.KindResult, Ok: &ok, Error: &bad}

	case xdr.ScSpecTypeScSpecTypeTuple:
		if t.Tuple == nil {
			return model.Type{}, errors.New("tuple type has no body")
		}
		elements := make([]model.Type, 0, len(t.Tuple.ValueTypes))
		for i := range t.Tuple.ValueTypes {
			e, err := convertType(t.Tuple.ValueTypes[i], depth+1)
			if err != nil {
				return model.Type{}, fmt.Errorf("tuple element %d: %w", i, err)
			}
			elements = append(elements, e)
		}
		out = model.Type{Kind: model.KindTuple, Elements: elements}

	case xdr.ScSpecTypeScSpecTypeBytesN:
		if t.BytesN == nil {
			return model.Type{}, errors.New("bytesN type has no body")
		}
		out = model.Type{Kind: model.KindBytesN, N: uint32(t.BytesN.N)}

	case xdr.ScSpecTypeScSpecTypeUdt:
		if t.Udt == nil {
			return model.Type{}, errors.New("udt type has no body")
		}
		out = model.Type{Kind: model.KindUDT, Name: t.Udt.Name}

	default:
		return model.Type{}, fmt.Errorf("unknown spec type %d", t.Type)
	}

	out.Display = model.Render(out)
	return out, nil
}
