package main

import (
	"bufio"
	"io"
)

// newBufReader buffers the vsock side: the host writes one frame per write, and reading
// several at once saves syscalls.
func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, 256<<10) }
