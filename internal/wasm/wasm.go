// Package wasm reads custom sections out of a WebAssembly module.
//
// Soroban embeds a contract's interface and build metadata in custom
// sections of its uploaded WASM ("contractspecv0" and "contractmetav0"
// respectively), so extracting them is the first step of decoding a
// contract's ABI. This package implements only as much of the binary format
// as that requires: it walks the top-level section framing and never parses
// section bodies, so it stays indifferent to WASM features and post-MVP
// proposals.
//
// The format is specified at https://webassembly.github.io/spec/core/binary/.
package wasm

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Section names Soroban uses.
const (
	// SectionContractSpec holds the stream of XDR-encoded SCSpecEntry values
	// describing the contract's interface (SEP-48).
	SectionContractSpec = "contractspecv0"
	// SectionContractMeta holds XDR-encoded SCMetaEntry key/value pairs
	// describing the toolchain that built the contract.
	SectionContractMeta = "contractmetav0"
	// SectionContractEnvMeta holds the interface version of the host
	// environment the contract was built against.
	SectionContractEnvMeta = "contractenvmetav0"
)

// MaxModuleSize bounds how large a WASM module this package will read.
//
// The Soroban network currently caps a contract's code at 128 KiB (a
// validator setting, so it can change). 2 MiB is sixteen times that: a
// module the network accepted is always decodable here, while a hostile or
// corrupt response cannot make SoroVault walk gigabytes of bytes.
const MaxModuleSize = 2 << 20

// ErrModuleTooLarge reports a module above MaxModuleSize. It is a property
// of the input, not a server fault, so the API reports it as 422.
var ErrModuleTooLarge = errors.New("wasm: module exceeds the size limit")

// ErrSectionNotFound is returned by CustomSection when the module parses
// cleanly but contains no custom section with the requested name.
var ErrSectionNotFound = errors.New("wasm: custom section not found")

// customSectionID is the section ID reserved for custom sections; every
// other ID denotes a known section whose body we skip over.
const customSectionID = 0

var wasmMagic = [4]byte{0x00, 0x61, 0x73, 0x6D}

// maxSectionNameLen bounds how large a custom section's name may be before
// we treat the module as malformed. Real names are a handful of bytes; the
// limit stops a corrupt length from driving a huge allocation.
const maxSectionNameLen = 1 << 16

// CustomSection returns the payload of the named custom section.
//
// If the module is well-formed but has no such section, it returns
// ErrSectionNotFound, which callers can test for with errors.Is. The
// returned slice aliases module rather than copying it.
func CustomSection(module []byte, name string) ([]byte, error) {
	body, err := afterHeader(module)
	if err != nil {
		return nil, err
	}

	for len(body) > 0 {
		id := body[0]
		body = body[1:]

		size, n, err := uvarint(body)
		if err != nil {
			return nil, fmt.Errorf("wasm: section %d size: %w", id, err)
		}
		body = body[n:]

		if uint64(len(body)) < size {
			return nil, fmt.Errorf("wasm: section %d truncated: want %d bytes, have %d", id, size, len(body))
		}
		payload := body[:size]
		body = body[size:]

		if id != customSectionID {
			continue
		}

		sectionName, rest, err := readName(payload)
		if err != nil {
			return nil, err
		}
		if sectionName == name {
			return rest, nil
		}
	}

	return nil, fmt.Errorf("%w: %q", ErrSectionNotFound, name)
}

// CustomSectionNames returns the names of every custom section in the
// module, in the order they appear. It is useful for diagnosing a contract
// whose spec section is missing.
func CustomSectionNames(module []byte) ([]string, error) {
	body, err := afterHeader(module)
	if err != nil {
		return nil, err
	}

	var names []string
	for len(body) > 0 {
		id := body[0]
		body = body[1:]

		size, n, err := uvarint(body)
		if err != nil {
			return nil, fmt.Errorf("wasm: section %d size: %w", id, err)
		}
		body = body[n:]

		if uint64(len(body)) < size {
			return nil, fmt.Errorf("wasm: section %d truncated: want %d bytes, have %d", id, size, len(body))
		}
		payload := body[:size]
		body = body[size:]

		if id != customSectionID {
			continue
		}
		name, _, err := readName(payload)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

// afterHeader validates the 8-byte module header and returns everything
// following it.
func afterHeader(module []byte) ([]byte, error) {
	if len(module) > MaxModuleSize {
		return nil, fmt.Errorf("%w: module is %d bytes, limit is %d", ErrModuleTooLarge, len(module), MaxModuleSize)
	}

	const headerLen = 8
	if len(module) < headerLen {
		return nil, fmt.Errorf("wasm: module too short: %d bytes", len(module))
	}
	if [4]byte(module[:4]) != wasmMagic {
		return nil, errors.New("wasm: bad magic, not a WebAssembly module")
	}
	if v := binary.LittleEndian.Uint32(module[4:8]); v != 1 {
		return nil, fmt.Errorf("wasm: unsupported binary format version %d", v)
	}
	return module[headerLen:], nil
}

// readName reads a length-prefixed UTF-8 name from the front of a custom
// section payload, returning the name and the bytes that follow it.
func readName(payload []byte) (string, []byte, error) {
	nameLen, n, err := uvarint(payload)
	if err != nil {
		return "", nil, fmt.Errorf("wasm: custom section name length: %w", err)
	}
	if nameLen > maxSectionNameLen {
		return "", nil, fmt.Errorf("wasm: custom section name too long: %d bytes", nameLen)
	}
	payload = payload[n:]
	if uint64(len(payload)) < nameLen {
		return "", nil, errors.New("wasm: custom section name truncated")
	}
	return string(payload[:nameLen]), payload[nameLen:], nil
}

// uvarint decodes an unsigned LEB128 integer, returning its value and the
// number of bytes consumed.
//
// The WASM encoding permits redundant padding bytes, so this accepts any
// encoding that fits in 32 bits rather than requiring the canonical minimal
// form. binary.Uvarint is not usable here: it reports truncation and
// overflow with the same zero-length signal.
func uvarint(b []byte) (uint64, int, error) {
	const maxBytes = 5 // ceil(32/7)

	var value uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		if i == maxBytes {
			return 0, 0, errors.New("uvarint overflows 32 bits")
		}
		c := b[i]
		value |= uint64(c&0x7F) << shift
		if c&0x80 == 0 {
			if value > 0xFFFFFFFF {
				return 0, 0, errors.New("uvarint overflows 32 bits")
			}
			return value, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, errors.New("uvarint truncated")
}
