//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "footprint: Windows only (it measures the WebView2 window)")
	os.Exit(2)
}
