//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "start: Windows only (it drives hyperbutler through HCS)")
	os.Exit(2)
}
