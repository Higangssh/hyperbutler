//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "soak: Windows only (it drives winbox and samples Windows perf counters)")
	os.Exit(2)
}
