//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "netfwd runs only in the Linux guest")
	os.Exit(1)
}
