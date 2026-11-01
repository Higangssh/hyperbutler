//go:build windows

package main

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTable = iphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	tcpTableOwnerPIDAll = 5 // TCP_TABLE_OWNER_PID_ALL
	udpTableOwnerPID    = 1 // UDP_TABLE_OWNER_PID
)

// tables reads the system's TCP and UDP tables, IPv4 and IPv6, with the owning PID of
// every row. It needs no privileges.
type tables struct {
	buf []uint64 // reused; uint64 keeps the rows aligned
}

func (t *tables) read() ([]Socket, error) {
	var out []Socket
	for _, q := range []struct {
		proto string
		proc  *windows.LazyProc
		af    uintptr
		class uintptr
	}{
		{"tcp4", procGetExtendedTcpTable, windows.AF_INET, tcpTableOwnerPIDAll},
		{"tcp6", procGetExtendedTcpTable, windows.AF_INET6, tcpTableOwnerPIDAll},
		{"udp4", procGetExtendedUdpTable, windows.AF_INET, udpTableOwnerPID},
		{"udp6", procGetExtendedUdpTable, windows.AF_INET6, udpTableOwnerPID},
	} {
		b, err := t.get(q.proc, q.af, q.class)
		if err != nil {
			return nil, fmt.Errorf("%s table: %w", q.proto, err)
		}
		ss, err := ParseTable(q.proto, b)
		if err != nil {
			return nil, fmt.Errorf("%s table: %w", q.proto, err)
		}
		out = append(out, ss...)
	}
	return out, nil
}

func (t *tables) get(proc *windows.LazyProc, af, class uintptr) ([]byte, error) {
	for range 8 {
		if len(t.buf) == 0 {
			t.buf = make([]uint64, 8<<10)
		}
		size := uint32(len(t.buf) * 8) //nolint:gosec // G115: the buffer stays far below 4 GiB
		r, _, _ := proc.Call(uintptr(unsafe.Pointer(&t.buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, class, 0)
		switch windows.Errno(r) {
		case 0:
			return unsafe.Slice((*byte)(unsafe.Pointer(&t.buf[0])), size), nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			t.buf = make([]uint64, size/8+1024)
		default:
			return nil, windows.Errno(r)
		}
	}
	return nil, errors.New("the table keeps growing")
}
