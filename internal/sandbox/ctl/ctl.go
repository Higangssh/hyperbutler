// Package ctl is the third channel to a sandbox session's guest agent (ADR 0009): the
// agent's terminals, and the project's git history in and out as bundles. It is separate
// from internal/sandbox/wire, which carries the review: nothing here reaches the user's
// files. Terminal output goes to the user's terminal and a bundle out goes to `git fetch`
// after CheckBundle.
//
// # Trust
//
// The guest runs the agent and is hostile. The host decodes only ReadFrame's bounded
// frames, and takes no length, name or code from the guest without a check. What the
// guest decodes comes from the host, and a guest that lies about it harms itself.
//
// # Wire format
//
// Every message is one frame: u32 BE n (1 <= n <= MaxBody), u8 type, then n-1 bytes. The
// connection's first frame comes from the host and says what it is for:
//
//	TypeTerm      JSON TermRequest    start or attach a terminal
//	TypeBundleIn  JSON BundleRequest  followed by Input frames, an empty one ends them
//	TypeBundleOut (empty)             the commits since the copy, as a bundle
//	TypeShutdown  (empty)             shut the session down in order; answered by TypeExit
//	TypeHead      (empty)             the commit HEAD is at, as Output, if it is past the copy
//
// Then, host to guest:
//
//	TypeInput     bytes               the terminal's keyboard, or the bundle
//	TypeResize    u16 cols, u16 rows  the terminal's size
//
// and guest to host:
//
//	TypeOutput    bytes               the terminal's screen, or the bundle
//	TypeExit      i32 code            the end: a process's exit code; see ExitNoCommits
//	TypeError     text                the end: why it could not be done, shown escaped
package ctl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// Port is the AF_VSOCK port a session's guest agent serves this protocol on.
	Port = 2392
	// MaxData bounds one frame's bytes, and MaxBody fits it with the type.
	MaxData = 64 << 10
	MaxBody = 1 + MaxData
	// MaxTerminals bounds a session's terminals at once.
	MaxTerminals = 8
	// MaxArgs and MaxEnv bound what a TermRequest may carry, in entries.
	MaxArgs = 256
	MaxEnv  = 64
	// ExitNoCommits is BundleOut's exit code when HEAD has nothing the copy did not.
	ExitNoCommits = 3
)

// Type is a frame's type.
type Type uint8

// The frame types.
const (
	TypeTerm Type = iota + 1
	TypeBundleIn
	TypeBundleOut
	TypeInput
	TypeResize
	TypeOutput
	TypeExit
	TypeError
	TypeShutdown
	TypeHead
)

// TermRequest starts a terminal, or attaches to one when Attach is set. Argv empty means
// a login shell. Env entries are NAME=value and travel only here: never on the kernel
// command line (ADR 0009, "Secrets").
type TermRequest struct {
	// Attach is the terminal's ID; "main" is the one `sandbox run` starts.
	Attach string   `json:"attach,omitempty"`
	ID     string   `json:"id,omitempty"`
	Argv   []string `json:"argv,omitempty"`
	Env    []string `json:"env,omitempty"`
	Cols   uint16   `json:"cols"`
	Rows   uint16   `json:"rows"`
}

// BundleRequest brings the project's history in. Head is the branch HEAD names, "" for a
// detached one at Commit. Name, Email and AutoCRLF go in the session's generated git
// config, which holds nothing else from the host: the files came from Windows, so git
// there must read their line ends as git here does, or every file looks modified.
type BundleRequest struct {
	Head     string `json:"head,omitempty"`
	Commit   string `json:"commit"`
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	AutoCRLF string `json:"autocrlf,omitempty"`
}

// ErrFrame is a frame outside the protocol's bounds.
var ErrFrame = errors.New("ctl: bad frame")

// WriteFrame writes one frame in a single Write. data must fit MaxData.
func WriteFrame(w io.Writer, t Type, data []byte) error {
	if len(data) > MaxData {
		return fmt.Errorf("%w: %d bytes", ErrFrame, len(data))
	}
	b := make([]byte, 5+len(data))
	binary.BigEndian.PutUint32(b, uint32(1+len(data))) //nolint:gosec // G115: bounded by MaxData above
	b[4] = byte(t)
	copy(b[5:], data)
	_, err := w.Write(b)
	return err
}

// ReadFrame reads one frame. It allocates only what a frame within MaxBody needs.
func ReadFrame(r io.Reader) (Type, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:4]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[:4])
	if n < 1 || n > MaxBody {
		return 0, nil, fmt.Errorf("%w: length %d", ErrFrame, n)
	}
	if _, err := io.ReadFull(r, h[4:]); err != nil {
		return 0, nil, unexpected(err)
	}
	data := make([]byte, n-1)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, unexpected(err)
	}
	return Type(h[4]), data, nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Resize encodes a terminal size for TypeResize.
func Resize(cols, rows uint16) []byte {
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, cols), rows)
}

// ParseResize decodes TypeResize's bytes.
func ParseResize(b []byte) (cols, rows uint16, err error) {
	if len(b) != 4 {
		return 0, 0, fmt.Errorf("%w: resize of %d bytes", ErrFrame, len(b))
	}
	return binary.BigEndian.Uint16(b), binary.BigEndian.Uint16(b[2:]), nil
}

// Exit encodes an exit code for TypeExit.
func Exit(code int32) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(code)) //nolint:gosec // G115: the code's bits, ParseExit undoes it
}

// ParseExit decodes TypeExit's bytes.
func ParseExit(b []byte) (int32, error) {
	if len(b) != 4 {
		return 0, fmt.Errorf("%w: exit of %d bytes", ErrFrame, len(b))
	}
	return int32(binary.BigEndian.Uint32(b)), nil //nolint:gosec // G115: the code's bits
}

// A bundle's head is bounded: it names one ref and the commits it builds on, and a
// session's HEAD has a few parents at most in any honest case.
const (
	maxBundleHead = 1 << 20
	maxPrereqs    = 4096
)

// ErrBundle is a bundle CheckBundle refuses.
var ErrBundle = errors.New("ctl: bad bundle")

// CheckBundle checks the head of a bundle from the guest before git sees it: a v2 or v3
// bundle with sha-1 or sha-256 object names, whose only ref is HEAD. It reads head, the
// file's first bytes (at most MaxBundleHead() of them is enough). Git checks the rest with
// fsck on, and fetches only HEAD whatever the file claims; the check keeps a bundle with
// a filter, an odd capability or other refs from reaching it at all.
func CheckBundle(head []byte) error {
	if len(head) > maxBundleHead {
		head = head[:maxBundleHead]
	}
	end := bytes.Index(head, []byte("\n\n"))
	if end < 0 {
		return fmt.Errorf("%w: no end of the head in %d bytes", ErrBundle, len(head))
	}
	lines := bytes.Split(head[:end], []byte("\n"))
	switch string(lines[0]) {
	case "# v2 git bundle", "# v3 git bundle":
	default:
		return fmt.Errorf("%w: not a git bundle", ErrBundle)
	}
	refs, prereqs := 0, 0
	for _, l := range lines[1:] {
		switch {
		case len(l) > 0 && l[0] == '@':
			if !bytes.Equal(l, []byte("@object-format=sha1")) && !bytes.Equal(l, []byte("@object-format=sha256")) {
				return fmt.Errorf("%w: a capability hyperbutler does not take", ErrBundle)
			}
		case len(l) > 0 && l[0] == '-':
			// "-<oid> <subject>": the subject is the guest's text and git ignores it.
			oid, _, _ := bytes.Cut(l[1:], []byte(" "))
			if !isOID(oid) {
				return fmt.Errorf("%w: a bad prerequisite", ErrBundle)
			}
			if prereqs++; prereqs > maxPrereqs {
				return fmt.Errorf("%w: too many prerequisites", ErrBundle)
			}
		default:
			oid, name, ok := bytes.Cut(l, []byte(" "))
			if !ok || !isOID(oid) || !bytes.Equal(name, []byte("HEAD")) {
				return fmt.Errorf("%w: a ref other than HEAD", ErrBundle)
			}
			refs++
		}
	}
	if refs != 1 {
		return fmt.Errorf("%w: %d refs", ErrBundle, refs)
	}
	return nil
}

// BundleCommit returns the commit a bundle CheckBundle took names as HEAD.
func BundleCommit(head []byte) string {
	if len(head) > maxBundleHead {
		head = head[:maxBundleHead]
	}
	end := bytes.Index(head, []byte("\n\n"))
	if end < 0 {
		return ""
	}
	for _, l := range bytes.Split(head[:end], []byte("\n")) {
		if oid, name, ok := bytes.Cut(l, []byte(" ")); ok && isOID(oid) && bytes.Equal(name, []byte("HEAD")) {
			return string(oid)
		}
	}
	return ""
}

// MaxBundleHead is how much of a bundle CheckBundle needs at most.
func MaxBundleHead() int { return maxBundleHead }

func isOID(b []byte) bool {
	if len(b) != 40 && len(b) != 64 {
		return false
	}
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
