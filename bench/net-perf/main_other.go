//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "net-perf runs on Windows against a hyperbutler engine")
	os.Exit(2)
}
