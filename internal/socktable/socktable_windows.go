//go:build windows

package socktable

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
	tcpTableOwnerPIDListener = 3 // TCP_TABLE_OWNER_PID_LISTENER
	tcpTableOwnerPIDAll      = 5 // TCP_TABLE_OWNER_PID_ALL
	udpTableOwnerPID         = 1 // UDP_TABLE_OWNER_PID
)

type query struct {
	proto string
	proc  *windows.LazyProc
	af    uintptr
	class uintptr
}

// Reader reads the system's socket tables. Its buffer is reused from read to read, so
// a Reader is not safe for concurrent use; the zero value is ready.
type Reader struct {
	buf []uint64 // uint64 keeps the rows aligned
}

// All returns every TCP and UDP socket, IPv4 and IPv6, with the owning PID of each.
func (t *Reader) All() ([]Socket, error) {
	return t.read([]query{
		{"tcp4", procGetExtendedTcpTable, windows.AF_INET, tcpTableOwnerPIDAll},
		{"tcp6", procGetExtendedTcpTable, windows.AF_INET6, tcpTableOwnerPIDAll},
		{"udp4", procGetExtendedUdpTable, windows.AF_INET, udpTableOwnerPID},
		{"udp6", procGetExtendedUdpTable, windows.AF_INET6, udpTableOwnerPID},
	})
}

// TCPListeners returns the TCP sockets in the LISTEN state, IPv4 and IPv6.
func (t *Reader) TCPListeners() ([]Socket, error) {
	return t.read([]query{
		{"tcp4", procGetExtendedTcpTable, windows.AF_INET, tcpTableOwnerPIDListener},
		{"tcp6", procGetExtendedTcpTable, windows.AF_INET6, tcpTableOwnerPIDListener},
	})
}

// UDP returns the bound UDP sockets, IPv4 and IPv6.
func (t *Reader) UDP() ([]Socket, error) {
	return t.read([]query{
		{"udp4", procGetExtendedUdpTable, windows.AF_INET, udpTableOwnerPID},
		{"udp6", procGetExtendedUdpTable, windows.AF_INET6, udpTableOwnerPID},
	})
}

func (t *Reader) read(qs []query) ([]Socket, error) {
	var out []Socket
	for _, q := range qs {
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

func (t *Reader) get(proc *windows.LazyProc, af, class uintptr) ([]byte, error) {
	if err := proc.Find(); err != nil {
		return nil, err
	}
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
