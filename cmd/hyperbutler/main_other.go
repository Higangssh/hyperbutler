//go:build !windows

// Command hyperbutler runs on Windows only.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "hyperbutler: runs on Windows only")
	os.Exit(1)
}
