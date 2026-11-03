//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "noadmin: Windows only (it probes named pipes and AF_HYPERV)")
	os.Exit(2)
}
