package broker

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// TestPipeClientCloseInterruptsRead: a Read waiting for a reply that never comes ends
// at once when another goroutine closes the client, and later calls fail cleanly.
func TestPipeClientCloseInterruptsRead(t *testing.T) {
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), ClientSID: currentSID(t), Instances: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialPipeClient(ctx, l.Name()) // no server reads or writes
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var b [8]byte
		_, err := c.Read(b[:])
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_ = c.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read after Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not end the read")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v", d)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after Close: %v", err)
	}
	_ = c.Close() // a second Close is fine
}

// TestPipeClientDeadline: a read past its deadline fails with a timeout.
func TestPipeClientDeadline(t *testing.T) {
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), ClientSID: currentSID(t), Instances: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := dialPipeClient(context.Background(), l.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var b [8]byte
	if _, err := c.Read(b[:]); !errors.Is(err, errTimeout) {
		t.Fatalf("read past the deadline: %v", err)
	}
}
