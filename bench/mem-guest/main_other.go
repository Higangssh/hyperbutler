//go:build !linux

// Command mem-guest is the in-container half of bench/mem-reclaim; it runs only in a
// Linux container (see main_linux.go).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "mem-guest: Linux only (it runs in the bench container)")
	os.Exit(2)
}
