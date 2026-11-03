// Package leaktest holds the goroutine-leak check shared by hyperbutler test
// binaries (ADR 0003, guardrail 1). Only _test.go files may import it, so
// goleak is never linked into a shipped binary.
package leaktest

import (
	"testing"

	"go.uber.org/goleak"
)

// iocpLoop is the entry function of internal/winipc's single I/O completion port
// goroutine.
//
// winipc starts it once per process, under a sync.Once, the first time any pipe or
// Hyper-V socket is opened (ioPort in internal/winipc/iocp_windows.go), and it never
// stops by design: it services every winipc handle for the life of the process. It is
// not a leak. Its top frame is a syscall (GetQueuedCompletionStatusEx), not this
// function, so goleak.IgnoreTopFunction cannot match it; IgnoreAnyFunction with the
// exact name is the narrowest option that does. No other goroutine has this function
// on its stack.
const iocpLoop = "github.com/Higangssh/hyperbutler/internal/winipc.ioCompletionLoop"

// IgnoreIOCP ignores winipc's process-lifetime completion-port goroutine. Use it only
// in packages whose tests open winipc pipes or hvsock handles.
var IgnoreIOCP = goleak.IgnoreAnyFunction(iocpLoop)

// VerifyTestMain runs the package's tests and then fails the binary if any
// goroutine other than those excused by opts is still running.
func VerifyTestMain(m *testing.M, opts ...goleak.Option) {
	goleak.VerifyTestMain(m, opts...)
}
