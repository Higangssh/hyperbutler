//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "hyperbutlerd runs in the Linux guest only")
	os.Exit(2)
}
