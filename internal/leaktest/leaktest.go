// Package leaktest holds the goroutine-leak check shared by hyperbutler test
// binaries (ADR 0003, guardrail 1). Only _test.go files may import it, so
// goleak is never linked into a shipped binary.
package leaktest

import (
	"testing"

	"go.uber.org/goleak"
)

// winioIOCP is the entry function of go-winio's single I/O completion port
// goroutine.
//
// go-winio starts it once per process, under a sync.Once, the first time any
// overlapped file or pipe is created (initIO in go-winio file.go), and it never
// stops by design: it services every winio handle for the life of the process.
// It is not a leak. Its top frame is a syscall (GetQueuedCompletionStatus), not
// this function, so goleak.IgnoreTopFunction cannot match it; IgnoreAnyFunction
// with the exact name is the narrowest option that does. No other goroutine has
// this function on its stack.
const winioIOCP = "github.com/Microsoft/go-winio.ioCompletionProcessor"

// IgnoreWinioIOCP ignores go-winio's process-lifetime completion-port goroutine.
// Use it only in packages whose tests open go-winio pipes or hvsock handles.
var IgnoreWinioIOCP = goleak.IgnoreAnyFunction(winioIOCP)

// VerifyTestMain runs the package's tests and then fails the binary if any
// goroutine other than those excused by opts is still running.
func VerifyTestMain(m *testing.M, opts ...goleak.Option) {
	goleak.VerifyTestMain(m, opts...)
}
