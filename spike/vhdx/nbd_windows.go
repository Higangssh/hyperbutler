package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

// A minimal NBD server (fixed newstyle handshake, simple replies) that serves one sparse
// raw file and turns NBD_CMD_TRIM / NBD_CMD_WRITE_ZEROES into FSCTL_SET_ZERO_DATA. It
// exists to test one idea: if QEMU's Windows file driver drops discards, can the VMM's
// block backend live in our own process instead? Protocol reference:
// https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md
// Not hardened: a real implementation needs fuzzing (CONTRIBUTING.md) and bounds on every field.

const (
	nbdMagic          = 0x4e42444d41474943 // "NBDMAGIC"
	nbdOptMagic       = 0x49484156454F5054 // "IHAVEOPT"
	nbdRepMagic       = 0x0003e889045565a9
	nbdReqMagic       = 0x25609513
	nbdSimpleRepMagic = 0x67446698

	nbdFlagFixedNewstyle = 1 << 0
	nbdFlagNoZeroes      = 1 << 1

	nbdFlagCFixedNewstyle = 1 << 0
	nbdFlagCNoZeroes      = 1 << 1

	nbdOptExportName = 1
	nbdOptAbort      = 2
	nbdOptGo         = 7

	nbdRepAck       = 1
	nbdRepInfo      = 3
	nbdRepErrUnsup  = 1<<31 + 1
	nbdInfoExport   = 0
	nbdFlagHasFlags = 1 << 0
	nbdFlagSendFl   = 1 << 2
	nbdFlagSendFUA  = 1 << 3
	nbdFlagSendTrim = 1 << 5
	nbdFlagSendWZ   = 1 << 6

	nbdCmdRead  = 0
	nbdCmdWrite = 1
	nbdCmdDisc  = 2
	nbdCmdFlush = 3
	nbdCmdTrim  = 4
	nbdCmdWZ    = 6

	nbdCmdFlagFUA    = 1 << 0
	nbdCmdFlagNoHole = 1 << 1

	nbdEIO    = 5
	nbdEINVAL = 22

	maxNBDRequest = 32 << 20
)

type nbdServer struct {
	ln   net.Listener
	f    *os.File
	size int64
	wg   sync.WaitGroup

	mu         sync.Mutex
	trims      int
	trimBytes  int64
	zeroes     int
	zeroBytes  int64
	writeBytes int64
}

func serveNBD(path string) (*nbdServer, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.Close()
		return nil, err
	}
	s := &nbdServer{ln: ln, f: f, size: st.Size()}
	// Connections are served one at a time, so requests from different clients can never
	// interleave and a trim can never overtake an earlier write. QEMU uses one connection.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if err := s.handle(c); err != nil && !errors.Is(err, io.EOF) {
				fmt.Println("  nbd:", err)
			}
			c.Close()
		}
	}()
	return s, nil
}

func (s *nbdServer) Close() {
	s.ln.Close()
	s.wg.Wait()
	s.mu.Lock()
	fmt.Printf("  nbd stats: %d trims (%s), %d write-zeroes (%s), %s written\n",
		s.trims, mib(s.trimBytes), s.zeroes, mib(s.zeroBytes), mib(s.writeBytes))
	s.mu.Unlock()
	s.f.Close()
}

func (s *nbdServer) handle(c net.Conn) error {
	be := binary.BigEndian
	hello := make([]byte, 18)
	be.PutUint64(hello[0:], nbdMagic)
	be.PutUint64(hello[8:], nbdOptMagic)
	be.PutUint16(hello[16:], nbdFlagFixedNewstyle|nbdFlagNoZeroes)
	if _, err := c.Write(hello); err != nil {
		return err
	}
	var clientFlags uint32
	if err := binary.Read(c, be, &clientFlags); err != nil {
		return err
	}
	if clientFlags&^(nbdFlagCFixedNewstyle|nbdFlagCNoZeroes) != 0 {
		return fmt.Errorf("unknown client flags 0x%x", clientFlags)
	}
	txFlags := uint16(nbdFlagHasFlags | nbdFlagSendFl | nbdFlagSendFUA | nbdFlagSendTrim | nbdFlagSendWZ)

	reply := func(opt, typ uint32, data []byte) error {
		b := make([]byte, 20+len(data))
		be.PutUint64(b[0:], nbdRepMagic)
		be.PutUint32(b[8:], opt)
		be.PutUint32(b[12:], typ)
		be.PutUint32(b[16:], uint32(len(data)))
		copy(b[20:], data)
		_, err := c.Write(b)
		return err
	}

	// Option haggling.
	for {
		var hdr [16]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return err
		}
		if be.Uint64(hdr[0:]) != nbdOptMagic {
			return fmt.Errorf("bad option magic")
		}
		opt, n := be.Uint32(hdr[8:]), be.Uint32(hdr[12:])
		if n > 4096 {
			return fmt.Errorf("option %d too long", opt)
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c, data); err != nil {
			return err
		}
		switch opt {
		case nbdOptExportName:
			b := make([]byte, 10, 10+124)
			be.PutUint64(b[0:], uint64(s.size))
			be.PutUint16(b[8:], txFlags)
			if clientFlags&nbdFlagCNoZeroes == 0 {
				b = append(b, make([]byte, 124)...)
			}
			if _, err := c.Write(b); err != nil {
				return err
			}
			return s.transmit(c)
		case nbdOptGo:
			info := make([]byte, 12)
			be.PutUint16(info[0:], nbdInfoExport)
			be.PutUint64(info[2:], uint64(s.size))
			be.PutUint16(info[10:], txFlags)
			if err := reply(opt, nbdRepInfo, info); err != nil {
				return err
			}
			if err := reply(opt, nbdRepAck, nil); err != nil {
				return err
			}
			return s.transmit(c)
		case nbdOptAbort:
			return reply(opt, nbdRepAck, nil)
		default:
			// Structured replies, meta contexts, extended headers: not needed here.
			if err := reply(opt, nbdRepErrUnsup, nil); err != nil {
				return err
			}
		}
	}
}

func (s *nbdServer) transmit(c net.Conn) error {
	be := binary.BigEndian
	var req [28]byte
	buf := make([]byte, maxNBDRequest)
	zeros := make([]byte, 1<<20)
	for {
		if _, err := io.ReadFull(c, req[:]); err != nil {
			return err
		}
		if be.Uint32(req[0:]) != nbdReqMagic {
			return fmt.Errorf("bad request magic")
		}
		flags, typ := be.Uint16(req[4:]), be.Uint16(req[6:])
		cookie := be.Uint64(req[8:])
		off, n := int64(be.Uint64(req[16:])), int64(be.Uint32(req[24:]))
		var errCode uint32
		var payload []byte
		inRange := nbdInRange(off, n, s.size)
		fua := flags&nbdCmdFlagFUA != 0

		switch typ {
		case nbdCmdRead:
			if !inRange || n > maxNBDRequest {
				errCode = nbdEINVAL
			} else if _, err := s.f.ReadAt(buf[:n], off); err != nil {
				errCode = nbdEIO
			} else {
				payload = buf[:n]
			}
		case nbdCmdWrite:
			if n > maxNBDRequest {
				return fmt.Errorf("write of %d bytes too large", n)
			}
			if _, err := io.ReadFull(c, buf[:n]); err != nil {
				return err
			}
			if !inRange {
				errCode = nbdEINVAL
			} else if _, err := s.f.WriteAt(buf[:n], off); err != nil {
				errCode = nbdEIO
			} else {
				s.count(&s.writeBytes, nil, n)
				if flags&nbdCmdFlagFUA != 0 && s.f.Sync() != nil {
					errCode = nbdEIO
				}
			}
		case nbdCmdFlush:
			if s.f.Sync() != nil {
				errCode = nbdEIO
			}
		case nbdCmdTrim:
			if !inRange {
				errCode = nbdEINVAL
			} else if err := zeroData(s.f, off, n); err != nil {
				errCode = nbdEIO
			} else if fua && s.f.Sync() != nil {
				errCode = nbdEIO
			} else {
				s.count(&s.trimBytes, &s.trims, n)
			}
		case nbdCmdWZ:
			if !inRange {
				errCode = nbdEINVAL
			} else if flags&nbdCmdFlagNoHole != 0 {
				for done := int64(0); done < n && errCode == 0; {
					k := min(n-done, int64(len(zeros)))
					if _, err := s.f.WriteAt(zeros[:k], off+done); err != nil {
						errCode = nbdEIO
					}
					done += k
				}
				if errCode == 0 && fua && s.f.Sync() != nil {
					errCode = nbdEIO
				}
			} else if err := zeroData(s.f, off, n); err != nil {
				errCode = nbdEIO
			} else if fua && s.f.Sync() != nil {
				errCode = nbdEIO
			} else {
				s.count(&s.zeroBytes, &s.zeroes, n)
			}
		case nbdCmdDisc:
			return nil
		default:
			// An unknown command may carry a payload we cannot size, so the stream is lost.
			return fmt.Errorf("unknown command %d", typ)
		}

		var rep [16]byte
		be.PutUint32(rep[0:], nbdSimpleRepMagic)
		be.PutUint32(rep[4:], errCode)
		be.PutUint64(rep[8:], cookie)
		if _, err := c.Write(rep[:]); err != nil {
			return err
		}
		if payload != nil {
			if _, err := c.Write(payload); err != nil {
				return err
			}
		}
	}
}

// nbdInRange reports whether [off, off+n) lies inside an export of the given size,
// without overflowing for offsets near 2^63.
func nbdInRange(off, n, size int64) bool {
	return off >= 0 && n >= 0 && off <= size && n <= size-off
}

func (s *nbdServer) count(bytes *int64, ops *int, n int64) {
	s.mu.Lock()
	*bytes += n
	if ops != nil {
		*ops++
	}
	s.mu.Unlock()
}
