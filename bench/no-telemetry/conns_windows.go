//go:build windows

package main

import "github.com/Higangssh/hyperbutler/internal/socktable"

// tables reads the system's TCP and UDP tables, IPv4 and IPv6, with the owning PID of
// every row. It needs no privileges.
type tables struct {
	r socktable.Reader
}

func (t *tables) read() ([]Socket, error) { return t.r.All() }
