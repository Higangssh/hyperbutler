package broker

import (
	"net"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// TestClientSlotVerbs: the slot methods put the slot on the wire, and the old methods
// send none (the Docker VM), as every older client does.
func TestClientSlotVerbs(t *testing.T) {
	a, b := net.Pipe()
	c := &Client{c: a, awake: func() time.Duration { return time.Duration(time.Now().UnixNano()) }}
	t.Cleanup(func() { _ = c.Close(); _ = b.Close() })
	got := make(chan proto.Request, 16)
	go func() {
		var buf [proto.MaxFrameSize]byte
		for {
			f, err := proto.ReadFrame(b, &buf)
			if err != nil {
				return
			}
			req, err := proto.DecodeRequest(f)
			if err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			got <- req
			r := proto.Reply{Type: proto.ReplyFlag | uint8(req.Op), State: proto.StateStopped, CPUs: 1, MemoryMB: proto.MinMemoryMB}
			if req.Op == proto.OpStop {
				r = proto.Reply{Type: proto.ReplyFlag | uint8(req.Op)}
			}
			out, err := proto.EncodeReply(r)
			if err != nil {
				t.Errorf("encode: %v", err)
				return
			}
			if _, err := b.Write(out); err != nil {
				return
			}
		}
	}()
	calls := []struct {
		do   func() error
		op   proto.Op
		slot uint8
	}{
		{func() error { _, err := c.Status(); return err }, proto.OpStatus, 0},
		{func() error { _, err := c.StatusSlot(3); return err }, proto.OpStatus, 3},
		{func() error { _, err := c.PauseSlot(2, true); return err }, proto.OpPause, 2},
		{func() error { return c.StopSlot(proto.MaxSlot, true) }, proto.OpStop, proto.MaxSlot},
		{func() error { return c.Stop(false) }, proto.OpStop, 0},
		{func() error { _, err := c.Resize(1, 2, 1024); return err }, proto.OpResize, 1},
	}
	for _, call := range calls {
		if err := call.do(); err != nil {
			t.Fatalf("%s: %v", call.op, err)
		}
		req := <-got
		if req.Op != call.op || req.Slot != call.slot {
			t.Errorf("sent %s slot %d, want %s slot %d", req.Op, req.Slot, call.op, call.slot)
		}
	}
}
