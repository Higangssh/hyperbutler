//go:build windows

package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

const fuzzSID = "S-1-5-21-1-2-3-1001"

// fuzzPeer is a Peer that never touches a real handle.
type fuzzPeer struct{ identifies int }

func (p *fuzzPeer) Identify() (Identity, error) {
	p.identifies++
	return Identity{SID: fuzzSID}, nil
}

func (p *fuzzPeer) DuplicateHandle(uint64) (windows.Handle, error) {
	return 0, errors.New("fuzz: no handles")
}

func (p *fuzzPeer) Impersonate(fn func() error) error { return fn() }

// fuzzConn reads the client stream from r and collects everything the server writes.
type fuzzConn struct {
	r   *bytes.Reader
	out bytes.Buffer
}

func (c *fuzzConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fuzzConn) Write(p []byte) (int, error) { return c.out.Write(p) }

func fuzzReq(r proto.Request) []byte {
	b, err := proto.EncodeRequest(r)
	if err != nil {
		panic(err)
	}
	return b
}

// fuzzModel is what the server must reply to one request, given that DuplicateHandle
// always fails (so no data location is ever set and no VM ever starts). It tracks the
// per-user CPU and memory settings the way resize changes them.
type fuzzModel struct{ cpus, mem uint32 }

func (m *fuzzModel) check(t *testing.T, req proto.Request, rep proto.Reply) {
	t.Helper()
	wantErr := func(code proto.Code) {
		t.Helper()
		if !rep.IsError() || rep.Code != code {
			t.Fatalf("%v: reply %+v, want error code %d", req.Op, rep, code)
		}
	}
	status := func() {
		t.Helper()
		want := proto.Reply{Type: proto.ReplyFlag | uint8(req.Op), State: proto.StateStopped, CPUs: m.cpus, MemoryMB: m.mem}
		if rep != want {
			t.Fatalf("%v: reply %+v, want %+v", req.Op, rep, want)
		}
	}
	switch req.Op {
	case proto.OpVersion:
		want := proto.Reply{Type: proto.ReplyFlag | uint8(req.Op), ProtoVersion: proto.Version, BrokerVersion: "fuzz-1"}
		if rep != want {
			t.Fatalf("version: reply %+v", rep)
		}
	case proto.OpStart:
		wantErr(proto.CodeNoDataLocation)
	case proto.OpStop, proto.OpPause:
		wantErr(proto.CodeNotRunning)
	case proto.OpStatus:
		status()
	case proto.OpSetDataLocation:
		wantErr(proto.CodeInvalidHandle)
	case proto.OpResize:
		cpus, mem := m.cpus, m.mem
		if req.CPUs != 0 {
			cpus = req.CPUs
		}
		if req.MemoryMB != 0 {
			mem = req.MemoryMB
		}
		if cpus < 1 || cpus > 8 || mem < 512 || mem > 16<<10 {
			wantErr(proto.CodeOutOfRange)
			return
		}
		m.cpus, m.mem = cpus, mem
		status()
	default:
		t.Fatalf("unexpected op %v", req.Op)
	}
}

// FuzzServeConn feeds arbitrary bytes to ServeConn as the client stream. ServeConn must
// return, never panic, and write only whole, valid reply frames: exactly one per request
// it decoded plus at most one error reply for the frame that ended the connection. Each
// reply must match a model of the server running on FakeBackend with a peer whose
// handles can never be duplicated.
func FuzzServeConn(f *testing.F) {
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	version := fuzzReq(proto.Request{Op: proto.OpVersion})
	status := fuzzReq(proto.Request{Op: proto.OpStatus})
	resize := fuzzReq(proto.Request{Op: proto.OpResize, CPUs: 4, MemoryMB: 4096})
	resizeBig := fuzzReq(proto.Request{Op: proto.OpResize, CPUs: proto.MaxCPUs})
	start := fuzzReq(proto.Request{Op: proto.OpStart})
	stop := fuzzReq(proto.Request{Op: proto.OpStop, Force: true})
	setDL := fuzzReq(proto.Request{Op: proto.OpSetDataLocation, Handle: 0x1a4})
	startNet := fuzzReq(proto.Request{Op: proto.OpStart, CPUs: 4, MemoryMB: 4096, Features: proto.FeatureNet | proto.FeatureShare,
		NetAddr: [4]byte{192, 168, 127, 0}, NetPrefix: 24, NetMTU: 9000, NetFlags: proto.NetOffload})
	pause := fuzzReq(proto.Request{Op: proto.OpPause})
	resume := fuzzReq(proto.Request{Op: proto.OpPause, Resume: true})
	f.Add([]byte{})
	f.Add(version)
	f.Add(status)
	f.Add(resize)
	f.Add(start)
	f.Add(stop)
	f.Add(setDL)
	f.Add(cat(version, status, resize, status, resizeBig, start, stop, setDL, start, status))
	f.Add(cat(status, []byte{0xff, 0xff}, status))                                                   // oversize length mid-stream
	f.Add(cat(status, []byte{0, 1, 1}))                                                              // length 1
	f.Add(cat(status, status[:3]))                                                                   // truncated frame
	f.Add(cat(status, []byte{0, 2, 2, byte(proto.OpStatus)}, status))                                // version 2
	f.Add(cat(status, []byte{0, 2, 1, 0x84}, status))                                                // reply as request
	f.Add(cat(status, []byte{0, 5, 1, byte(proto.OpStop), 1, 1, 0}))                                 // bool false encoded
	f.Add([]byte{0, 6, 1, byte(proto.OpSetDataLocation), 4, 8})                                      // truncated handle
	f.Add(cat([]byte{0, 12, 1, byte(proto.OpSetDataLocation), 4, 8}, bytes.Repeat([]byte{0xff}, 8))) // pseudo-handle
	f.Add(cat(status, []byte{0}))                                                                    // half a length prefix
	f.Add(startNet)
	f.Add(cat(pause, resume, startNet, status, pause))
	f.Add(cat(status, []byte{0, 5, 1, byte(proto.OpPause), 17, 1, 0}))    // resume false encoded
	f.Add(cat(status, []byte{0, 5, 1, byte(proto.OpStart), 12, 1, 0x80})) // unknown feature

	f.Fuzz(func(t *testing.T, in []byte) {
		s, err := New(Config{Backend: NewFakeBackend(), Version: "fuzz-1"})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		conn := &fuzzConn{r: bytes.NewReader(in)}
		peer := &fuzzPeer{}
		done := make(chan error, 1)
		go func() { done <- s.ServeConn(context.Background(), conn, peer) }()
		var serveErr error
		select {
		case serveErr = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("ServeConn did not return for %x", in)
		}

		// Work out what the server should have done, frame by frame.
		var (
			buf      [proto.MaxFrameSize]byte
			reqs     []proto.Request
			framesIn int
			endsBad  bool // the stream ends with a frame that earns an error reply
			endCode  proto.Code
			cleanEOF bool
		)
		in2 := bytes.NewReader(in)
	parse:
		for {
			b, err := proto.ReadFrame(in2, &buf)
			switch {
			case errors.Is(err, io.EOF):
				cleanEOF = true
				break parse
			case errors.Is(err, proto.ErrFrameTooLarge), errors.Is(err, proto.ErrInvalid):
				endsBad, endCode = true, proto.CodeBadRequest
				break parse
			case err != nil:
				break parse
			}
			framesIn++
			req, err := proto.DecodeRequest(b)
			if err != nil {
				endsBad, endCode = true, proto.CodeBadRequest
				if errors.Is(err, proto.ErrUnsupportedVersion) {
					endCode = proto.CodeUnsupportedVersion
				}
				break parse
			}
			reqs = append(reqs, req)
		}
		if cleanEOF != (serveErr == nil) {
			t.Fatalf("ServeConn(%x) = %v; clean EOF expected: %v", in, serveErr, cleanEOF)
		}
		if want := min(framesIn, 1); peer.identifies != want {
			t.Fatalf("Identify called %d times for %d frames", peer.identifies, framesIn)
		}

		out := conn.out.Bytes()
		if len(out) > (framesIn+1)*proto.MaxFrameSize {
			t.Fatalf("%d bytes written for %d frames", len(out), framesIn)
		}
		var replies []proto.Reply
		or := bytes.NewReader(out)
		for {
			b, err := proto.ReadFrame(or, &buf)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("output %x does not split into frames: %v", out, err)
			}
			rep, err := proto.DecodeReply(b)
			if err != nil {
				t.Fatalf("server wrote invalid reply %x: %v", b, err)
			}
			replies = append(replies, rep)
		}
		if len(replies) > framesIn+1 {
			t.Fatalf("%d replies for %d request frames", len(replies), framesIn)
		}
		want := len(reqs)
		if endsBad {
			want++
		}
		if len(replies) != want {
			t.Fatalf("input %x: %d replies, want %d (%d requests, bad end %v)", in, len(replies), want, len(reqs), endsBad)
		}
		m := fuzzModel{cpus: 2, mem: 2048}
		for i, req := range reqs {
			m.check(t, req, replies[i])
		}
		if endsBad {
			last := replies[len(replies)-1]
			if !last.IsError() || last.Code != endCode {
				t.Fatalf("final reply %+v, want error code %d", last, endCode)
			}
		}
	})
}
