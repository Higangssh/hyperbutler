//go:build !linux

package main

import (
	"fmt"
	"os"
)

// The agent runs only in the Linux guest. This stub keeps `go build ./...` and the relay
// tests working on the Windows host.
func main() {
	fmt.Fprintln(os.Stderr, "winbox-agent runs in the Linux guest only (GOOS=linux)")
	os.Exit(1)
}
