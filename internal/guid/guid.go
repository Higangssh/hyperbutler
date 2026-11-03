// Package guid is the GUID type hyperbutler uses for VM, runtime and Hyper-V socket
// service IDs, with only the standard library: the broker service may depend on nothing
// else (ADR 0004 rule 10).
//
// GUID has the layout of Windows' GUID (and of golang.org/x/sys/windows.GUID), so either
// converts to it with a plain conversion. Its text form is the usual 36 characters,
// lower case: 01234567-89ab-cdef-0123-456789abcdef.
package guid

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 4122 version 5 is defined over SHA-1
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// GUID is a 128-bit identifier in Windows' layout: Data1 to Data3 are integers (stored
// little-endian in memory on Windows), Data4 is bytes.
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// ErrSyntax is returned for a string that is not a GUID's text form.
var ErrSyntax = errors.New("guid: not of the form xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx")

// NewV4 is a random (version 4) GUID.
func NewV4() (GUID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return GUID{}, err
	}
	return FromArray(versioned(b, 4)), nil
}

// NewV5 is the name-based (version 5, SHA-1) GUID of name in namespace (RFC 4122).
func NewV5(namespace GUID, name []byte) (GUID, error) {
	ns := namespace.ToArray()
	h := sha1.New() //nolint:gosec // G401: as above
	_, _ = h.Write(ns[:])
	_, _ = h.Write(name)
	var b [16]byte
	copy(b[:], h.Sum(nil))
	return FromArray(versioned(b, 5)), nil
}

// versioned sets the version nibble and the RFC 4122 variant bits of b.
func versioned(b [16]byte, version byte) [16]byte {
	b[6] = b[6]&0x0f | version<<4
	b[8] = b[8]&0x3f | 0x80
	return b
}

// FromArray reads a GUID from its 16 bytes in big-endian (RFC 4122, network) order.
func FromArray(b [16]byte) GUID { return fromBytes(b, binary.BigEndian) }

// ToArray is g's 16 bytes in big-endian (RFC 4122, network) order.
func (g GUID) ToArray() [16]byte { return g.bytes(binary.BigEndian) }

// FromWindowsArray reads a GUID from its 16 bytes in Windows' in-memory order (Data1 to
// Data3 little-endian).
func FromWindowsArray(b [16]byte) GUID { return fromBytes(b, binary.LittleEndian) }

// ToWindowsArray is g's 16 bytes in Windows' in-memory order.
func (g GUID) ToWindowsArray() [16]byte { return g.bytes(binary.LittleEndian) }

func fromBytes(b [16]byte, order binary.ByteOrder) GUID {
	g := GUID{Data1: order.Uint32(b[0:4]), Data2: order.Uint16(b[4:6]), Data3: order.Uint16(b[6:8])}
	copy(g.Data4[:], b[8:])
	return g
}

func (g GUID) bytes(order binary.ByteOrder) [16]byte {
	var b [16]byte
	order.PutUint32(b[0:4], g.Data1)
	order.PutUint16(b[4:6], g.Data2)
	order.PutUint16(b[6:8], g.Data3)
	copy(b[8:], g.Data4[:])
	return b
}

// String is g's text form, lower case, without braces.
func (g GUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x", g.Data1, g.Data2, g.Data3, g.Data4[:2], g.Data4[2:])
}

// FromString parses the 36-character text form (either case, no braces).
func FromString(s string) (GUID, error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return GUID{}, ErrSyntax
	}
	var b [16]byte
	// The hex runs between the dashes, as byte offsets into b.
	for _, r := range [...]struct{ from, to, at int }{{0, 8, 0}, {9, 13, 4}, {14, 18, 6}, {19, 23, 8}, {24, 36, 10}} {
		if _, err := hex.Decode(b[r.at:], []byte(s[r.from:r.to])); err != nil {
			return GUID{}, ErrSyntax
		}
	}
	return FromArray(b), nil
}

// MarshalText is String's text, for encoding/json and the like.
func (g GUID) MarshalText() ([]byte, error) { return []byte(g.String()), nil }

// UnmarshalText parses what MarshalText writes.
func (g *GUID) UnmarshalText(text []byte) error {
	v, err := FromString(string(text))
	if err != nil {
		return err
	}
	*g = v
	return nil
}
