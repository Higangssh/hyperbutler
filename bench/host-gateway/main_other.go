//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "host-gateway runs on Windows against a hyperbutler engine")
	os.Exit(2)
}
