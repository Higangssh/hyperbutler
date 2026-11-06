//go:build !windows

package main

import "errors"

func runCmd([]string) error { return errors.New("run: HCS is Windows-only") }

func sandboxCmd([]string) error { return errors.New("sandbox: HCS is Windows-only") }
