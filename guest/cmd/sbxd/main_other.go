//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "sbxd runs in a sandbox session VM only")
	os.Exit(2)
}
