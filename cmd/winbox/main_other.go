//go:build !windows

// Command winbox runs on Windows only.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "winbox: runs on Windows only")
	os.Exit(1)
}
