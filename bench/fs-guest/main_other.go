//go:build !linux

// Command fs-guest runs inside a Linux container; see main_linux.go.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fs-guest runs in a Linux container (GOOS=linux)")
	os.Exit(2)
}
