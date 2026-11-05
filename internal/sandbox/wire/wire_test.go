package wire

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, m := range []Msg{
		{Type: TypeList, Seq: 1},
		{Type: TypeRead, Seq: 2, Off: 1 << 40, Len: MaxData, Path: "src/main.go"},
		{Type: TypeEntry, Seq: 3, Kind: Added, Flags: FlagExec, Size: 12, Path: "bin/x"},
		{Type: TypeEntry, Seq: 3, Kind: Modified, Flags: FlagLink, Path: "l", Link: "../t"},
		{Type: TypeEntry, Seq: 3, Kind: Deleted, Path: strings.Repeat("a", MaxPath)},
		{Type: TypeEnd, Seq: 3, Count: 3},
		{Type: TypeData, Seq: 4, EOF: true, Data: []byte("hello")},
		{Type: TypeData, Seq: 4, Data: bytes.Repeat([]byte{1}, MaxData)},
		{Type: TypeError, Seq: 5, Errno: 2, Text: "no such file"},
	} {
		b, err := Encode(m)
		if err != nil {
			t.Fatalf("encode %+v: %v", m.Type, err)
		}
		got, err := Decode(b[4:], m.Type&0x80 != 0)
		if err != nil {
			t.Fatalf("decode %#x: %v", m.Type, err)
		}
		if got.Type != m.Type || got.Path != m.Path || got.Link != m.Link || got.Count != m.Count ||
			got.EOF != m.EOF || !bytes.Equal(got.Data, m.Data) || got.Text != m.Text || got.Off != m.Off {
			t.Errorf("%#x: round trip changed it", m.Type)
		}
	}
	for _, m := range []Msg{
		{Type: TypeRead, Len: 0, Path: "a"},
		{Type: TypeRead, Len: MaxData + 1, Path: "a"},
		{Type: TypeRead, Len: 1},
		{Type: TypeEntry, Kind: 9, Path: "a"},
		{Type: TypeEntry, Kind: Added},
		{Type: TypeEntry, Kind: Added, Flags: FlagLink, Path: "a"},
		{Type: TypeEntry, Kind: Added, Path: "a", Link: "b"},
		{Type: TypeEntry, Kind: Added, Flags: 0x80, Path: "a"},
		{Type: TypeEnd, Count: MaxEntries + 1},
		{Type: TypeError},
		{Type: 0x7f},
	} {
		if _, err := Encode(m); err == nil {
			t.Errorf("encoded %+v", m)
		}
	}
	// The wrong direction is refused.
	b, _ := Encode(Msg{Type: TypeList})
	if _, err := Decode(b[4:], true); !errors.Is(err, ErrFrame) {
		t.Errorf("a host request accepted from the guest: %v", err)
	}
}

// guest answers requests on its end of a pipe with fn.
func guest(t *testing.T, fn func(req Msg) []Msg) *Client {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		buf := make([]byte, MaxBody)
		for {
			body, err := ReadFrame(b, buf)
			if err != nil {
				return
			}
			req, err := Decode(body, false)
			if err != nil {
				return
			}
			for _, r := range fn(req) {
				r.Seq = req.Seq
				out, err := Encode(r)
				if err != nil {
					t.Errorf("guest encode: %v", err)
					return
				}
				if _, err := b.Write(out); err != nil {
					return
				}
			}
		}
	}()
	return NewClient(a)
}

func TestClientList(t *testing.T) {
	c := guest(t, func(Msg) []Msg {
		return []Msg{{Type: TypeEntry, Kind: Added, Path: "a"}, {Type: TypeEntry, Kind: Deleted, Path: "b"}, {Type: TypeEnd, Count: 2}}
	})
	got, err := c.List()
	if err != nil || len(got) != 2 {
		t.Fatalf("List = %v, %v", got, err)
	}
	for name, reply := range map[string][]Msg{
		"count mismatch": {{Type: TypeEntry, Kind: Added, Path: "a"}, {Type: TypeEnd, Count: 5}},
		"duplicate":      {{Type: TypeEntry, Kind: Added, Path: "a"}, {Type: TypeEntry, Kind: Modified, Path: "a"}, {Type: TypeEnd, Count: 2}},
		"data in list":   {{Type: TypeData, EOF: true}},
	} {
		c := guest(t, func(Msg) []Msg { return reply })
		if _, err := c.List(); !errors.Is(err, ErrFrame) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClientReadFile(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 300000) // 3 MB, several reads
	c := guest(t, func(req Msg) []Msg {
		end := min(int(req.Off)+int(req.Len), len(content))
		return []Msg{{Type: TypeData, Data: content[req.Off:end], EOF: end == len(content)}}
	})
	got, err := c.ReadFile("f", 10<<20)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("ReadFile: %d bytes, %v", len(got), err)
	}
	if _, err := c.ReadFile("f", 1<<20); err == nil {
		t.Error("read past the limit")
	}
	// A guest that never says EOF is cut off at the limit.
	endless := guest(t, func(req Msg) []Msg { return []Msg{{Type: TypeData, Data: make([]byte, req.Len)}} })
	if _, err := endless.ReadFile("f", 3<<20); err == nil {
		t.Error("an endless file was read")
	}
	stuck := guest(t, func(Msg) []Msg { return []Msg{{Type: TypeData}} })
	if _, err := stuck.ReadFile("f", 1<<20); !errors.Is(err, ErrFrame) {
		t.Errorf("empty reads: %v", err)
	}
	failing := guest(t, func(Msg) []Msg { return []Msg{{Type: TypeError, Errno: 13, Text: string(rune(0x202e)) + "denied"}} })
	var ge *GuestError
	if _, err := failing.ReadFile("f", 1); !errors.As(err, &ge) || ge.Errno != 13 || strings.Contains(err.Error(), "denied") {
		t.Errorf("guest error: %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	for _, m := range []Msg{
		{Type: TypeEntry, Kind: Added, Flags: FlagLink, Path: "a", Link: "b"},
		{Type: TypeData, EOF: true, Data: []byte("x")},
		{Type: TypeError, Errno: 1, Text: "e"},
		{Type: TypeRead, Len: 1, Path: "p"},
		{Type: TypeEnd, Count: 1},
	} {
		b, _ := Encode(m)
		f.Add(b[4:])
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, fromGuest := range []bool{true, false} {
			m, err := Decode(body, fromGuest)
			if err != nil {
				continue
			}
			again, err := Encode(m)
			if err != nil || !bytes.Equal(again[4:], body) {
				t.Fatalf("decoded %+v does not re-encode to the same body: %v", m, err)
			}
			if len(m.Path) > MaxPath || len(m.Link) > MaxPath || len(m.Data) > MaxData || len(m.Text) > MaxMsg {
				t.Fatalf("a field over its limit: %+v", m)
			}
		}
	})
}
