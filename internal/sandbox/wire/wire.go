// Package wire is the protocol the engine uses to read a sandbox session's changes out
// of its VM (ADR 0009, "Bringing changes out"): the host asks the session's guest agent
// for the list of files that differ from what was copied in, then reads the content of
// the ones the review shows. The host never mounts or parses the session's file system.
//
// # Trust
//
// The guest runs the agent and is hostile. Everything the host decodes is bounded (frame
// size, path length, entry count, total bytes) and decoded canonically: a frame decodes
// only if encoding the result gives back the same bytes, which the fuzz targets check.
// A path is only bytes here; the review checks it against its grammar
// (review.CheckPath) and shows it escaped. Sizes and hashes the guest reports are hints
// for display: the host hashes the bytes it read itself.
//
// # Wire format
//
// Every message is one frame:
//
//	u32 BE  n        body length, HeaderLen <= n <= MaxBody
//	body:
//	  u8      version  Version (1)
//	  u8      type     a Type valid for the sender's direction
//	  u16 BE  reserved 0
//	  u32 BE  seq      the host request's sequence number, echoed by every reply to it
//	  payload          per type, below
//
// Host to guest:
//
//	TypeList   (empty)                    list the changes; answered by Entry frames and
//	                                      one End, or one Error
//	TypeRead   u64 off, u32 len, path     read len (1..MaxData) bytes of path at off;
//	                                      answered by one Data or one Error
//
// Guest to host:
//
//	TypeEntry  u8 kind, u8 flags, u64 size, u16 n, path[n], u16 m, link[m]
//	TypeEnd    u32 count                  the number of Entry frames sent
//	TypeData   u8 eof, bytes              eof is 1 when the file ends within this read
//	TypeError  u16 errno, u16 n, msg[n]   a Linux errno and text, shown escaped
package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Version   = 1
	HeaderLen = 8
	// Port is the AF_VSOCK port a session's guest agent answers List and Read on, and
	// CopyPort the one it takes the project's tar stream on, once.
	Port     = 2390
	CopyPort = 2391
	// MaxData bounds one read's bytes; MaxBody fits it with its header.
	MaxData = 1 << 20
	MaxBody = HeaderLen + 1 + MaxData
	// MaxPath bounds a path on the wire, in bytes (review.MaxPath is in UTF-16 units,
	// at most 3 bytes each in UTF-8 for the characters it allows, so 4 KiB is enough).
	MaxPath = 4096
	// MaxMsg bounds an error's text.
	MaxMsg = 256
	// MaxEntries bounds one list.
	MaxEntries = 100000
)

// Type is a message type.
type Type uint8

const (
	TypeList  Type = 1
	TypeRead  Type = 2
	TypeEntry Type = 0x81
	TypeEnd   Type = 0x82
	TypeData  Type = 0x83
	TypeError Type = 0x84
)

// Kind is what happened to a file.
type Kind uint8

const (
	Added    Kind = 1
	Modified Kind = 2
	Deleted  Kind = 3
)

// Entry flags.
const (
	FlagExec = 1 << 0 // the file has an execute bit
	FlagLink = 1 << 1 // the file is a symbolic link; Link is its target
)

// Msg is one decoded frame. Only the fields of its Type are set.
type Msg struct {
	Type Type
	Seq  uint32

	// TypeRead.
	Off uint64
	Len uint32
	// TypeRead and TypeEntry.
	Path string

	// TypeEntry.
	Kind  Kind
	Flags uint8
	Size  uint64
	Link  string

	// TypeEnd.
	Count uint32

	// TypeData.
	EOF  bool
	Data []byte

	// TypeError.
	Errno uint16
	Text  string
}

// ErrFrame is wrapped by every decoding error.
var ErrFrame = errors.New("bad sandbox frame")

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrFrame, fmt.Sprintf(format, a...))
}

// Encode returns m as one frame.
func Encode(m Msg) ([]byte, error) {
	var p []byte
	switch m.Type {
	case TypeList:
	case TypeRead:
		if m.Len == 0 || m.Len > MaxData || len(m.Path) == 0 || len(m.Path) > MaxPath {
			return nil, bad("read: len %d, path %d bytes", m.Len, len(m.Path))
		}
		p = binary.BigEndian.AppendUint64(p, m.Off)
		p = binary.BigEndian.AppendUint32(p, m.Len)
		p = append(p, m.Path...)
	case TypeEntry:
		if m.Kind < Added || m.Kind > Deleted || m.Flags&^(FlagExec|FlagLink) != 0 ||
			len(m.Path) == 0 || len(m.Path) > MaxPath || len(m.Link) > MaxPath ||
			(m.Flags&FlagLink == 0) != (m.Link == "") {
			return nil, bad("entry: kind %d, flags %#x, path %d bytes, link %d bytes", m.Kind, m.Flags, len(m.Path), len(m.Link))
		}
		p = append(p, byte(m.Kind), m.Flags)
		p = binary.BigEndian.AppendUint64(p, m.Size)
		p = binary.BigEndian.AppendUint16(p, uint16(len(m.Path))) //nolint:gosec // G115: at most MaxPath
		p = append(p, m.Path...)
		p = binary.BigEndian.AppendUint16(p, uint16(len(m.Link))) //nolint:gosec // G115: at most MaxPath
		p = append(p, m.Link...)
	case TypeEnd:
		if m.Count > MaxEntries {
			return nil, bad("end: count %d", m.Count)
		}
		p = binary.BigEndian.AppendUint32(p, m.Count)
	case TypeData:
		if len(m.Data) > MaxData {
			return nil, bad("data: %d bytes", len(m.Data))
		}
		eof := byte(0)
		if m.EOF {
			eof = 1
		}
		p = append(p, eof)
		p = append(p, m.Data...)
	case TypeError:
		if m.Errno == 0 || m.Errno > 4095 || len(m.Text) > MaxMsg {
			return nil, bad("error: errno %d, %d bytes", m.Errno, len(m.Text))
		}
		p = binary.BigEndian.AppendUint16(p, m.Errno)
		p = binary.BigEndian.AppendUint16(p, uint16(len(m.Text))) //nolint:gosec // G115: at most MaxMsg
		p = append(p, m.Text...)
	default:
		return nil, bad("type %#x", m.Type)
	}
	n := HeaderLen + len(p)
	b := make([]byte, 4, 4+n)
	binary.BigEndian.PutUint32(b, uint32(n)) //nolint:gosec // G115: at most MaxBody
	b = append(b, Version, byte(m.Type), 0, 0)
	b = binary.BigEndian.AppendUint32(b, m.Seq)
	return append(b, p...), nil
}

// ReadFrame reads one frame body from r into buf (which must hold MaxBody bytes).
func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n < HeaderLen || n > MaxBody || int(n) > len(buf) {
		return nil, bad("length %d", n)
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// Decode decodes a frame body. fromGuest says which direction's types are accepted.
// Data's bytes alias body.
func Decode(body []byte, fromGuest bool) (Msg, error) {
	if len(body) < HeaderLen || len(body) > MaxBody {
		return Msg{}, bad("body %d bytes", len(body))
	}
	if body[0] != Version || body[2] != 0 || body[3] != 0 {
		return Msg{}, bad("version %d, reserved %#x", body[0], body[2:4])
	}
	m := Msg{Type: Type(body[1]), Seq: binary.BigEndian.Uint32(body[4:8])}
	if (m.Type&0x80 != 0) != fromGuest {
		return Msg{}, bad("type %#x from the wrong side", m.Type)
	}
	p := body[HeaderLen:]
	var err error
	switch m.Type {
	case TypeList:
		if len(p) != 0 {
			err = bad("list: %d payload bytes", len(p))
		}
	case TypeRead:
		if len(p) < 13 {
			return Msg{}, bad("read: %d bytes", len(p))
		}
		m.Off, m.Len, m.Path = binary.BigEndian.Uint64(p), binary.BigEndian.Uint32(p[8:]), string(p[12:])
	case TypeEntry:
		if len(p) < 14 {
			return Msg{}, bad("entry: %d bytes", len(p))
		}
		m.Kind, m.Flags, m.Size = Kind(p[0]), p[1], binary.BigEndian.Uint64(p[2:])
		n := int(binary.BigEndian.Uint16(p[10:]))
		if 12+n+2 > len(p) {
			return Msg{}, bad("entry: path %d bytes of %d", n, len(p))
		}
		m.Path = string(p[12 : 12+n])
		k := int(binary.BigEndian.Uint16(p[12+n:]))
		if 14+n+k != len(p) {
			return Msg{}, bad("entry: link %d bytes of %d", k, len(p))
		}
		m.Link = string(p[14+n:])
	case TypeEnd:
		if len(p) != 4 {
			return Msg{}, bad("end: %d bytes", len(p))
		}
		m.Count = binary.BigEndian.Uint32(p)
	case TypeData:
		if len(p) < 1 || p[0] > 1 {
			return Msg{}, bad("data: eof byte")
		}
		m.EOF, m.Data = p[0] == 1, p[1:]
	case TypeError:
		if len(p) < 4 {
			return Msg{}, bad("error: %d bytes", len(p))
		}
		m.Errno = binary.BigEndian.Uint16(p)
		n := int(binary.BigEndian.Uint16(p[2:]))
		if 4+n != len(p) {
			return Msg{}, bad("error: text %d bytes of %d", n, len(p))
		}
		m.Text = string(p[4:])
	default:
		return Msg{}, bad("type %#x", m.Type)
	}
	if err != nil {
		return Msg{}, err
	}
	// Canonical: the fields' own limits hold (Encode checks them) and re-encoding gives
	// the same bytes.
	again, err := Encode(m)
	if err != nil {
		return Msg{}, err
	}
	if !bytes.Equal(again[4:], body) {
		return Msg{}, bad("not canonical")
	}
	return m, nil
}
