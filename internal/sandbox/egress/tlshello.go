package egress

import "github.com/Higangssh/hyperbutler/internal/netstack"

// parseState is how far a parser got with the bytes so far. Once a prefix is decided,
// more bytes cannot change the decision (FuzzClientHello, FuzzHTTPHead).
type parseState int

const (
	parseBad parseState = iota
	parseMore
	parseDone
)

// parsed is a parser's result: on parseDone the name asked for, as sent, and for a hello
// where its last record ends; on parseBad a Kind for the log.
type parsed struct {
	state parseState
	name  string
	kind  string
	end   int
}

const (
	tlsRecordHandshake  = 22
	tlsRecordHeaderLen  = 5
	tlsMaxRecordLen     = 1 << 14
	tlsHandshakeHello   = 1
	tlsHandshakeHdrLen  = 4
	tlsMaxSessionID     = 32
	tlsVersionLow       = 0x0301 // TLS 1.0, the oldest with extensions as a norm
	tlsVersionHigh      = 0x0304 // TLS 1.3; a ClientHello's own version field stays at 1.2
	tlsExtServerName    = 0x0000
	tlsExtECH           = 0xfe0d
	tlsExtESNI          = 0xffce // the draft ECH grew from; old clients still send it
	tlsExtECHDraftLow   = 0xfe08 // drafts of ECH on the wire before the final number
	tlsExtECHDraftHigh  = 0xfe0c
	tlsNameTypeHostName = 0
	// maxHelloBytes bounds everything looked at, record headers included. A hello that
	// has not finished by then is denied.
	maxHelloBytes = netstack.MaxInspectBytes
)

func helloBad(kind string) parsed { return parsed{state: parseBad, kind: kind} }

// parseClientHello reads a TLS ClientHello from the start of a stream, reassembled from
// as many records and reads as it came in, and returns its one server name. It refuses
// what a server might read differently from it: anything but handshake records before
// the hello ends, bytes after the hello in its last record, repeated extensions, inner
// lengths that do not add up, SSLv2 hellos. It decides only on the whole hello: an ECH
// extension can come after the server name.
func parseClientHello(b []byte) parsed {
	full := len(b) >= maxHelloBytes
	if full {
		b = b[:maxHelloBytes]
	}
	whole := len(b)
	var hs []byte
	first := true
	for {
		if len(b) == 0 {
			return helloMoreOrFull(full)
		}
		if b[0] != tlsRecordHandshake {
			if first {
				return helloBad(KindNotTLS) // SSLv2 hellos, HTTP, anything else
			}
			return helloBad(KindBadHello) // a record of another kind inside the hello
		}
		if len(b) >= 2 && b[1] != 3 {
			return helloBad(KindNotTLS)
		}
		if len(b) >= 3 && (b[2] < 1 || b[2] > 4) {
			return helloBad(KindNotTLS)
		}
		if len(b) < tlsRecordHeaderLen {
			return helloMoreOrFull(full)
		}
		declared := int(b[3])<<8 | int(b[4])
		if declared == 0 || declared > tlsMaxRecordLen {
			return helloBad(KindBadHello)
		}
		frag := b[tlsRecordHeaderLen:]
		complete := len(frag) >= declared
		if complete {
			frag = frag[:declared]
		}
		if hs == nil {
			hs = frag[:len(frag):len(frag)] // appending to it copies, not overwrites b
		} else {
			hs = append(hs, frag...)
		}
		first = false
		if len(hs) >= 1 && hs[0] != tlsHandshakeHello {
			return helloBad(KindBadHello)
		}
		if len(hs) >= tlsHandshakeHdrLen {
			need := tlsHandshakeHdrLen + (int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3]))
			if need > maxHelloBytes {
				return helloBad(KindBadHello)
			}
			// The whole record counts, read or not: nothing may follow the hello in it.
			if len(hs)+declared-len(frag) > need {
				return helloBad(KindBadHello)
			}
			if complete && len(hs) == need {
				res := parseHelloBody(hs[tlsHandshakeHdrLen:])
				res.end = whole - (len(b) - tlsRecordHeaderLen - declared)
				return res
			}
		}
		if !complete {
			return helloMoreOrFull(full)
		}
		b = b[tlsRecordHeaderLen+declared:]
	}
}

// pipelinedHello reports whether the bytes behind a hello hold a handshake record, read
// the way a server frames them. Early data and a ChangeCipherSpec may follow a hello; a
// second hello may not. A record cut short ends the walk: what the guest sends after it
// is not read until the server has answered, and after a HelloRetryRequest it does not
// parse as a hello.
func pipelinedHello(b []byte) bool {
	for len(b) > 0 {
		if b[0] == tlsRecordHandshake {
			return true
		}
		if len(b) < tlsRecordHeaderLen {
			return false
		}
		n := tlsRecordHeaderLen + (int(b[3])<<8 | int(b[4]))
		if n > len(b) {
			return false
		}
		b = b[n:]
	}
	return false
}

// tlsRecordChangeCipher is the one record a client may send between its two hellos.
const tlsRecordChangeCipher = 20

// parseRetryHello reads what the guest sends after a HelloRetryRequest: at most one
// ChangeCipherSpec record of the one legal form, then a ClientHello by parseClientHello's
// rules. Anything else fails: a client has no reason to send it.
func parseRetryHello(b []byte) parsed {
	if len(b) > 0 && b[0] == tlsRecordChangeCipher {
		const ccsLen = tlsRecordHeaderLen + 1
		for i, c := range b[:min(len(b), ccsLen)] {
			switch i {
			case 1:
				if c != 3 {
					return helloBad(KindBadHello)
				}
			case 2:
				if c < 1 || c > 4 {
					return helloBad(KindBadHello)
				}
			case 3:
				if c != 0 {
					return helloBad(KindBadHello)
				}
			case 4, 5:
				if c != 1 {
					return helloBad(KindBadHello)
				}
			}
		}
		if len(b) < ccsLen {
			return parsed{state: parseMore}
		}
		b = b[ccsLen:]
	}
	return parseClientHello(b)
}

// What parseServerReply can say about the first bytes from the server.
type replyState int

const (
	replyPass replyState = iota
	replyMore
	replyRetry
)

const (
	tlsHandshakeServerHello = 2
	// tlsServerRandomEnd is where the random of a ServerHello ends: the handshake header,
	// the version and 32 bytes.
	tlsServerRandomEnd = tlsHandshakeHdrLen + 2 + 32
)

// tlsRetryRandom is the fixed random that makes a ServerHello a HelloRetryRequest (RFC
// 8446, section 4.1.3): the SHA-256 of "HelloRetryRequest".
var tlsRetryRandom = [32]byte{
	0xcf, 0x21, 0xad, 0x74, 0xe5, 0x9a, 0x61, 0x11, 0xbe, 0x1d, 0x8c, 0x02, 0x1e, 0x65, 0xb8, 0x91,
	0xc2, 0xa2, 0x11, 0x16, 0x7a, 0xbb, 0x8c, 0x5e, 0x07, 0x9e, 0x09, 0xe2, 0xc8, 0xa8, 0x33, 0x9c,
}

// parseServerReply looks for a HelloRetryRequest at the start of what a server sends. It
// reassembles the handshake as far as the random, whatever the records, so that a server
// cannot hide one by cutting it small. Anything that is not on its way to being a
// ServerHello is not this parser's concern and passes at once: a server that speaks first,
// an alert. So it never waits on anything but a handshake record cut short.
func parseServerReply(b []byte) replyState {
	var hs []byte
	for {
		if len(b) == 0 {
			return replyMore
		}
		if b[0] != tlsRecordHandshake || (len(b) >= 2 && b[1] != 3) {
			return replyPass
		}
		if len(b) < tlsRecordHeaderLen {
			return replyMore
		}
		declared := int(b[3])<<8 | int(b[4])
		if declared == 0 || declared > tlsMaxRecordLen {
			return replyPass
		}
		frag := b[tlsRecordHeaderLen:]
		frag = frag[:min(len(frag), declared)]
		if hs == nil {
			hs = frag[:len(frag):len(frag)] // appending to it copies, not overwrites b
		} else {
			hs = append(hs, frag...)
		}
		if len(hs) >= 1 && hs[0] != tlsHandshakeServerHello {
			return replyPass
		}
		if len(hs) >= tlsServerRandomEnd {
			if [32]byte(hs[tlsServerRandomEnd-32:tlsServerRandomEnd]) == tlsRetryRandom {
				return replyRetry
			}
			return replyPass
		}
		if len(frag) < declared {
			return replyMore
		}
		b = b[tlsRecordHeaderLen+declared:]
	}
}

func helloMoreOrFull(full bool) parsed {
	if full {
		return helloBad(KindBadHello)
	}
	return parsed{state: parseMore}
}

// tlsReader reads the big-endian fields of a hello; a read past the end reports false.
type tlsReader []byte

func (r *tlsReader) take(n int) ([]byte, bool) {
	if len(*r) < n {
		return nil, false
	}
	out := (*r)[:n]
	*r = (*r)[n:]
	return out, true
}

func (r *tlsReader) u8() (int, bool) {
	b, ok := r.take(1)
	if !ok {
		return 0, false
	}
	return int(b[0]), true
}

func (r *tlsReader) u16() (int, bool) {
	b, ok := r.take(2)
	if !ok {
		return 0, false
	}
	return int(b[0])<<8 | int(b[1]), true
}

// vec8 and vec16 read a vector with a 1- or 2-byte length in front.
func (r *tlsReader) vec8() ([]byte, bool) {
	n, ok := r.u8()
	if !ok {
		return nil, false
	}
	return r.take(n)
}

func (r *tlsReader) vec16() ([]byte, bool) {
	n, ok := r.u16()
	if !ok {
		return nil, false
	}
	return r.take(n)
}

func parseHelloBody(body []byte) parsed {
	r := tlsReader(body)
	version, ok := r.u16()
	if !ok || version < tlsVersionLow || version > tlsVersionHigh {
		return helloBad(KindBadHello)
	}
	if _, ok := r.take(32); !ok { // random
		return helloBad(KindBadHello)
	}
	session, ok := r.vec8()
	if !ok || len(session) > tlsMaxSessionID {
		return helloBad(KindBadHello)
	}
	suites, ok := r.vec16()
	if !ok || len(suites) == 0 || len(suites)%2 != 0 {
		return helloBad(KindBadHello)
	}
	methods, ok := r.vec8()
	if !ok || len(methods) == 0 {
		return helloBad(KindBadHello)
	}
	if len(r) == 0 {
		return helloBad(KindNoServerName) // no extensions at all
	}
	exts, ok := r.vec16()
	if !ok || len(r) != 0 {
		return helloBad(KindBadHello)
	}
	return parseExtensions(exts)
}

func parseExtensions(exts []byte) parsed {
	r := tlsReader(exts)
	seen := make(map[int]bool, 16)
	name := ""
	found := false
	for len(r) > 0 {
		typ, ok := r.u16()
		if !ok {
			return helloBad(KindBadHello)
		}
		data, ok := r.vec16()
		if !ok || seen[typ] {
			return helloBad(KindBadHello)
		}
		seen[typ] = true
		switch {
		case typ == tlsExtECH, typ == tlsExtESNI, typ >= tlsExtECHDraftLow && typ <= tlsExtECHDraftHigh:
			return helloBad(KindECH)
		case typ == tlsExtServerName:
			if name, ok = parseServerName(data); !ok {
				return helloBad(KindBadHello)
			}
			found = true
		}
	}
	if !found {
		return helloBad(KindNoServerName)
	}
	return parsed{state: parseDone, name: name}
}

// parseServerName reads the server_name extension: a list with exactly one host_name.
func parseServerName(data []byte) (string, bool) {
	r := tlsReader(data)
	list, ok := r.vec16()
	if !ok || len(r) != 0 {
		return "", false
	}
	l := tlsReader(list)
	typ, ok := l.u8()
	if !ok || typ != tlsNameTypeHostName {
		return "", false
	}
	name, ok := l.vec16()
	if !ok || len(name) == 0 || len(l) != 0 {
		return "", false
	}
	return string(name), true
}
