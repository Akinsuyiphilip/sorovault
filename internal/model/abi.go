// Package model defines SoroVault's contract interface (ABI) representation.
//
// These types are the project's public contract: they are what the HTTP API
// serves, what is stored as jsonb in Postgres, and what other tooling is
// expected to consume. They deliberately depend on nothing else in the
// project (and on no SDK types) so that consumers can vendor or re-implement
// them cheaply.
package model

import (
	"sort"
	"strconv"
)

// ABIVersion is the schema version of the JSON emitted by this package.
// It is bumped when a change would break existing consumers.
const ABIVersion = "1"

// Interface is a contract's complete decoded interface.
type Interface struct {
	ABIVersion string `json:"abi_version"`

	// Meta holds the contract's build metadata, decoded from the WASM's
	// "contractmetav0" custom section. Typical keys are "rsver" (the Rust
	// compiler version) and "rssdkver" (the soroban-sdk version). It is nil
	// when the contract carries no metadata section.
	Meta map[string]string `json:"meta,omitempty"`

	// EnvMeta holds the contract environment metadata, decoded from the
	// WASM's "contractenvmetav0" custom section: the host environment the
	// contract was built against. It is nil when the contract carries no
	// such section.
	EnvMeta *EnvMeta `json:"env_meta,omitempty"`

	Functions []Function `json:"functions"`
	Types     Types      `json:"types"`
	Events    []Event    `json:"events"`
}

// EnvMeta is the contract environment metadata: the version of the host
// environment a contract was built against.
//
// The host checks a contract's interface version before it runs the
// contract, so this says which protocol the contract needs: it runs on the
// protocol it was built for and on any later one.
type EnvMeta struct {
	// InterfaceVersion is the version of the host interface the contract
	// targets, written the way a version number is: the protocol version
	// on its own for a release ("25"), and with the pre-release number
	// after a dot for anything else ("25.1").
	//
	// The section carries the interface version as two numbers rather than
	// as this string; see Protocol and PreRelease for the split. Build
	// the value with NewEnvMeta, so the two cannot disagree.
	InterfaceVersion string `json:"interface_version"`

	// Protocol is the protocol version the contract was built against,
	// which is the lowest protocol it will run on: a host runs a contract
	// built for the same or an older protocol version.
	Protocol uint32 `json:"protocol,omitempty"`

	// PreRelease is the pre-release number of the protocol version. It is
	// zero for a released protocol, and any two different pre-release
	// numbers are incompatible, so a contract has to be recompiled.
	PreRelease uint32 `json:"pre_release,omitempty"`
}

// NewEnvMeta builds the environment metadata for a protocol version and a
// pre-release number, filling in the interface version that goes with them.
func NewEnvMeta(protocol, preRelease uint32) EnvMeta {
	meta := EnvMeta{Protocol: protocol, PreRelease: preRelease}
	meta.InterfaceVersion = strconv.FormatUint(uint64(protocol), 10)
	if preRelease != 0 {
		meta.InterfaceVersion += "." + strconv.FormatUint(uint64(preRelease), 10)
	}
	return meta
}

// Function is a single callable contract entry point.
type Function struct {
	Name string `json:"name"`
	Doc  string `json:"doc,omitempty"`

	Inputs []Param `json:"inputs"`

	// Outputs is a list because the XDR models it as a variable-length
	// array, but the spec constrains it to at most one entry: an empty
	// Outputs means the function returns nothing.
	Outputs []Type `json:"outputs"`
}

// Param is a named function input or event parameter.
type Param struct {
	Name string `json:"name"`
	Doc  string `json:"doc,omitempty"`
	Type Type   `json:"type"`
}

// Types groups the user-defined types a contract's interface refers to.
type Types struct {
	Structs    []Struct    `json:"structs"`
	Unions     []Union     `json:"unions"`
	Enums      []Enum      `json:"enums"`
	ErrorEnums []ErrorEnum `json:"error_enums"`
}

// Struct is a user-defined record type.
//
// Soroban encodes tuple structs (those with positional rather than named
// fields) as a struct whose field names are the decimal indices "0", "1",
// and so on; IsTuple reports that case so consumers need not re-derive it.
type Struct struct {
	Name    string        `json:"name"`
	Doc     string        `json:"doc,omitempty"`
	Lib     string        `json:"lib,omitempty"`
	Fields  []StructField `json:"fields"`
	IsTuple bool          `json:"is_tuple"`
}

// StructField is one named field of a Struct.
type StructField struct {
	Name string `json:"name"`
	Doc  string `json:"doc,omitempty"`
	Type Type   `json:"type"`
}

// Union is a user-defined sum type: a set of named cases, each of which may
// carry positional payload values.
type Union struct {
	Name  string      `json:"name"`
	Doc   string      `json:"doc,omitempty"`
	Lib   string      `json:"lib,omitempty"`
	Cases []UnionCase `json:"cases"`
}

// UnionCase is one variant of a Union. Values is empty for a void case.
type UnionCase struct {
	Name   string `json:"name"`
	Doc    string `json:"doc,omitempty"`
	Values []Type `json:"values"`
}

// Enum is a user-defined C-style enumeration over uint32 values.
type Enum struct {
	Name  string     `json:"name"`
	Doc   string     `json:"doc,omitempty"`
	Lib   string     `json:"lib,omitempty"`
	Cases []EnumCase `json:"cases"`
}

// EnumCase is one named value of an Enum.
type EnumCase struct {
	Name  string `json:"name"`
	Doc   string `json:"doc,omitempty"`
	Value uint32 `json:"value"`
}

// ErrorEnum is a user-defined enumeration of contract error codes.
type ErrorEnum struct {
	Name  string     `json:"name"`
	Doc   string     `json:"doc,omitempty"`
	Lib   string     `json:"lib,omitempty"`
	Cases []EnumCase `json:"cases"`
}

// Event is a contract event the contract may publish.
type Event struct {
	Name   string   `json:"name"`
	Doc    string   `json:"doc,omitempty"`
	Lib    string   `json:"lib,omitempty"`
	Prefix []string `json:"prefix_topics,omitempty"`

	Params []EventParam `json:"params"`

	// DataFormat describes how the non-topic parameters are packed into the
	// event's data payload: "single-value", "vec", or "map".
	DataFormat string `json:"data_format"`
}

// EventParam is one parameter of an Event.
type EventParam struct {
	Name string `json:"name"`
	Doc  string `json:"doc,omitempty"`
	Type Type   `json:"type"`

	// Location is "topic-list" if the parameter is published as an event
	// topic, or "data" if it forms part of the data payload.
	Location string `json:"location"`
}

// Function returns the named function and whether it was found.
func (i *Interface) Function(name string) (Function, bool) {
	for _, fn := range i.Functions {
		if fn.Name == name {
			return fn, true
		}
	}
	return Function{}, false
}

// SymbolNames lists the names the interface declares at the top level:
// functions, user-defined types and events. It is sorted and free of
// duplicates, and is what registry search matches against.
func (i *Interface) SymbolNames() []string {
	seen := make(map[string]bool)
	add := func(name string) {
		if name != "" {
			seen[name] = true
		}
	}
	for _, fn := range i.Functions {
		add(fn.Name)
	}
	for _, s := range i.Types.Structs {
		add(s.Name)
	}
	for _, u := range i.Types.Unions {
		add(u.Name)
	}
	for _, e := range i.Types.Enums {
		add(e.Name)
	}
	for _, e := range i.Types.ErrorEnums {
		add(e.Name)
	}
	for _, ev := range i.Events {
		add(ev.Name)
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
