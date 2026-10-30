//go:build !windows

package main

import "errors"

func runCmd([]string) error { return errors.New("run: HCS is Windows-only") }
