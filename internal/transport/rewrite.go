package transport

// The request filter: the one place the proxy looks inside HTTP (ADR 0007, integration
// step 3). With Proxy.RewriteCreate set, the client-to-guest direction follows request
// boundaries so that POST /containers/create bodies can be rewritten (Windows bind
// sources become guest paths). Every other byte is forwarded unchanged:
//
//   - Request heads and bodies of all other requests are copied verbatim. Bodies are
//     delimited the way dockerd's net/http server delimits them (Content-Length, or the
//     chunked framing, parsed with Go's own rules), so keep-alive and pipelined requests
//     after them are still seen.
//   - A request that may take the connection over (Upgrade, or the attach and exec
//     endpoints, which dockerd hijacks even without Upgrade) switches the direction to
//     plain byte copying for the rest of the connection, as does anything the filter
//     cannot follow (a malformed or oversized head, malformed chunking, HTTP/2). Then a
//     later create on that connection is not rewritten: dockerd sees the Windows path
//     and rejects it, which fails safe.
//   - The guest-to-client direction is never parsed.
//
// A rewritten create is sent with a correct Content-Length (a chunked body is
// de-chunked) and without Expect. A create the filter refuses (too large, folded
// header lines, or a rewrite error) is answered by the proxy itself, after dockerd has
// answered every earlier request on the connection, and the connection is closed.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// CreateRewriter rewrites the JSON body of a container create request. It returns the
// body to send (body itself when nothing changes) or an error the client is shown.
type CreateRewriter func(body []byte) ([]byte, error)

// MaxCreateBody bounds the container create body the proxy buffers for a rewrite.
// Larger bodies are refused with 413.
const MaxCreateBody = 4 << 20

const (
	// maxHeadBytes matches net/http's DefaultMaxHeaderBytes (plus its 4 KiB slack): a
	// longer head is something dockerd would refuse too, so it is passed on raw.
	maxHeadBytes = 1<<20 + 4096
	// maxChunkLine is net/http's limit on a chunk-size line.
	maxChunkLine = 4096
	// maxRejectDrain bounds how much of a refused request is read and discarded, so a
	// client still writing its body gets to read the error.
	maxRejectDrain = 64 << 20
	filterBufSize  = 64 << 10
)

var (
	// createPath matches POST /containers/create with or without the API version
	// prefix (dockerd's router accepts /v{version:[0-9.]+}).
	createPath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)
	// hijackPath matches the endpoints dockerd may hijack without an Upgrade header.
	hijackPath = regexp.MustCompile(`^(/v[0-9.]+)?/(containers/[^/]+/attach(/ws)?|exec/[^/]+/start|session|grpc)$`)

	errLineTooLong = errors.New("transport: line too long")
	errMalformed   = errors.New("transport: malformed chunked body")
)

// rejection is a create request the proxy answers itself.
type rejection struct {
	status int
	msg    string
}

func (r *rejection) Error() string { return r.msg }

// response is the HTTP response for r, in dockerd's error format.
func (r *rejection) response() []byte {
	body, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{"hyperbutler: " + r.msg})
	body = append(body, '\n')
	return fmt.Appendf(nil, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		r.status, http.StatusText(r.status), len(body), body)
}

// reqFilter is the client-to-guest direction of one connection.
type reqFilter struct {
	br      *bufio.Reader
	dst     io.Writer
	rewrite CreateRewriter
	buf     []byte // for raw copying
	// handled counts the requests followed to their end; wentRaw is set once the rest
	// of the connection is copied unchanged (for tests).
	handled int
	wentRaw bool
}

func newReqFilter(src io.Reader, dst io.Writer, rw CreateRewriter) *reqFilter {
	return &reqFilter{br: bufio.NewReaderSize(src, filterBufSize), dst: dst, rewrite: rw}
}

// run forwards requests until the client finishes sending (nil), a create is refused
// (*rejection), or a copy fails.
func (f *reqFilter) run() error {
	for ; ; f.handled++ {
		head, err := readHead(f.br)
		if err != nil {
			if len(head) > 0 {
				if _, werr := f.dst.Write(head); werr != nil {
					return werr
				}
			}
			switch {
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				return nil // the client finished (possibly mid-head; dockerd sees that too)
			case errors.Is(err, errLineTooLong):
				return f.raw()
			}
			return err
		}
		req, perr := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
		if perr != nil || req.ProtoMajor != 1 {
			// Whatever dockerd makes of it, it gets exactly these bytes.
			if _, err := f.dst.Write(head); err != nil {
				return err
			}
			return f.raw()
		}
		chunked := len(req.TransferEncoding) > 0
		if req.Method == http.MethodPost && createPath.MatchString(req.URL.Path) {
			if err := f.create(head, req, chunked); err != nil {
				return err
			}
			continue
		}
		if _, err := f.dst.Write(head); err != nil {
			return err
		}
		if hijacks(req) {
			detached, err := f.detachedExec(req, chunked)
			if err != nil {
				return err
			}
			if !detached {
				return f.raw()
			}
			continue
		}
		if chunked {
			err := scanChunked(f.br, f.dst, nil)
			switch {
			case err == nil:
				continue
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				return nil
			case errors.Is(err, errMalformed), errors.Is(err, errLineTooLong):
				return f.raw()
			}
			return err
		}
		if req.ContentLength > 0 {
			if _, err := f.copyN(req.ContentLength); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
}

// raw copies the rest of the connection unchanged.
func (f *reqFilter) raw() error {
	f.wentRaw = true
	if f.buf == nil {
		f.buf = make([]byte, copyBufferSize)
	}
	_, err := io.CopyBuffer(writerOnly{f.dst}, readerOnly{f.br}, f.buf)
	return err
}

func (f *reqFilter) copyN(n int64) (int64, error) {
	if f.buf == nil {
		f.buf = make([]byte, copyBufferSize)
	}
	w, err := io.CopyBuffer(writerOnly{f.dst}, io.LimitReader(f.br, n), f.buf)
	if err == nil && w < n {
		err = io.EOF
	}
	return w, err
}

// maxExecStartBody bounds the exec start body read to tell a detached start.
const maxExecStartBody = 64 << 10

// execStartPath matches POST /exec/{id}/start.
var execStartPath = regexp.MustCompile(`^(/v[0-9.]+)?/exec/[^/]+/start$`)

// detachedExec forwards the body of an exec start that hijacks only by its path (no
// Upgrade) and reports whether it is detached ({"Detach":true}): dockerd answers that
// with a plain response and keeps the connection, so the filter keeps following it
// (the Docker client sends a detached exec start as a plain POST on a pooled
// connection). Anything else may be hijacked and goes raw; the head was written
// already and the body has not been read then.
func (f *reqFilter) detachedExec(req *http.Request, chunked bool) (bool, error) {
	if req.Method != http.MethodPost || !execStartPath.MatchString(req.URL.Path) || upgrades(req) ||
		chunked || req.ContentLength <= 0 || req.ContentLength > maxExecStartBody {
		return false, nil
	}
	body := make([]byte, req.ContentLength)
	n, err := io.ReadFull(f.br, body)
	if _, werr := f.dst.Write(body[:n]); werr != nil {
		return false, werr
	}
	if err != nil {
		return false, err
	}
	var start struct{ Detach bool }
	if json.Unmarshal(body, &start) != nil || !start.Detach {
		return false, nil
	}
	return true, nil
}

// upgrades reports whether req asks to switch protocols.
func upgrades(req *http.Request) bool {
	if req.Method == http.MethodConnect || req.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range req.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// hijacks reports whether dockerd may take the connection over after req.
func hijacks(req *http.Request) bool {
	return upgrades(req) || hijackPath.MatchString(req.URL.Path)
}

// create reads, rewrites and forwards one container create request.
func (f *reqFilter) create(head []byte, req *http.Request, chunked bool) error {
	if folded(head) {
		return &rejection{http.StatusBadRequest, "container create: folded header lines are not supported"}
	}
	var body []byte
	if chunked {
		var b limitedBuffer
		b.max = MaxCreateBody
		if err := scanChunked(f.br, io.Discard, &b); err != nil {
			if errors.Is(err, errBodyTooLarge) {
				return &rejection{http.StatusRequestEntityTooLarge, fmt.Sprintf("container create body exceeds %d MiB", MaxCreateBody>>20)}
			}
			if errors.Is(err, errMalformed) || errors.Is(err, errLineTooLong) {
				return &rejection{http.StatusBadRequest, "container create: malformed chunked body"}
			}
			return err
		}
		body = b.b
	} else {
		if req.ContentLength > MaxCreateBody {
			return &rejection{http.StatusRequestEntityTooLarge, fmt.Sprintf("container create body exceeds %d MiB", MaxCreateBody>>20)}
		}
		body = make([]byte, req.ContentLength)
		if _, err := io.ReadFull(f.br, body); err != nil {
			return err
		}
	}
	nb, err := f.rewrite(body)
	if err != nil {
		return &rejection{http.StatusBadRequest, err.Error()}
	}
	if !chunked && bytes.Equal(nb, body) {
		// Nothing to translate: the request goes out exactly as it came.
		if _, err := f.dst.Write(head); err != nil {
			return err
		}
		_, err := f.dst.Write(body)
		return err
	}
	out := append(withContentLength(head, len(nb)), nb...)
	_, err = f.dst.Write(out)
	return err
}

// folded reports whether a head uses obsolete line folding (a header line starting with
// a space or tab), which withContentLength could not edit safely.
func folded(head []byte) bool {
	lines := bytes.SplitAfter(head, []byte("\n"))
	for _, l := range lines[1:] {
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') {
			return true
		}
	}
	return false
}

// withContentLength returns head without its Content-Length, Transfer-Encoding and
// Expect lines and with Content-Length: n. head is a complete, parsed request head
// without folded lines.
func withContentLength(head []byte, n int) []byte {
	lines := bytes.SplitAfter(head, []byte("\n"))
	out := make([]byte, 0, len(head)+32)
	out = append(out, lines[0]...)
	for _, l := range lines[1:] {
		if len(l) == 0 {
			continue
		}
		if isBlankLine(l) {
			break
		}
		// Exactly as net/http keys it: "Content-Length :" is another header to it
		// (FuzzRequestFilter), so it is kept.
		name, _, _ := bytes.Cut(l, []byte(":"))
		switch strings.ToLower(string(name)) {
		case "content-length", "transfer-encoding", "expect":
			continue
		}
		out = append(out, l...)
	}
	out = append(out, "Content-Length: "...)
	out = strconv.AppendInt(out, int64(n), 10)
	return append(out, "\r\n\r\n"...)
}

func isBlankLine(l []byte) bool {
	return len(l) == 1 && l[0] == '\n' || len(l) == 2 && l[0] == '\r' && l[1] == '\n'
}

// readHead reads one request head, through the blank line that ends it, as net/http's
// textproto reader delimits it (lines end in LF, an optional CR before it). It returns
// what it read with io.EOF if the client stopped first, or errLineTooLong past
// maxHeadBytes.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	lineStart := 0
	for {
		frag, err := br.ReadSlice('\n')
		head = append(head, frag...)
		if len(head) > maxHeadBytes {
			return head, errLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(head) > 0 {
				return head, io.ErrUnexpectedEOF
			}
			return head, err
		}
		// A blank first line is no request line: ReadRequest refuses it and the filter
		// goes raw.
		if isBlankLine(head[lineStart:]) {
			return head, nil
		}
		lineStart = len(head)
	}
}

// readLine reads one LF-terminated line of at most max bytes.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) >= max {
			return line, errLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// scanChunked reads one chunked body from br. framing receives every byte read except
// the chunk payloads; payload receives those, or framing does if payload is nil (then
// framing gets the body verbatim).
//
// It follows net/http's chunked reader (net/http/internal/chunked.go) and is never more
// lenient: lines end in CRLF with no other CR, sizes are 1 to 16 hex digits after
// trailing whitespace and any extension are dropped, chunk data ends in CRLF, and
// non-data overhead is bounded the same way. Trailers are not supported at all (nobody
// sends them to dockerd). What it refuses gives errMalformed, so the filter never frames
// a body differently from dockerd: it goes raw, or refuses the create.
func scanChunked(br *bufio.Reader, framing, payload io.Writer) error {
	if payload == nil {
		payload = framing
	}
	buf := make([]byte, 32<<10)
	var excess int64
	for {
		line, err := readLine(br, maxChunkLine)
		if _, werr := framing.Write(line); werr != nil {
			return werr
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if i := bytes.IndexByte(line, '\r'); i < 0 || i != len(line)-2 {
			return errMalformed // a bare LF, or a CR inside the line
		}
		excess += int64(len(line))
		n, ok := parseChunkSize(line[:len(line)-2])
		if !ok {
			return errMalformed
		}
		excess = max(excess-16-2*n, 0)
		if excess > 16*1024 {
			return errMalformed
		}
		if n == 0 {
			break
		}
		w, err := io.CopyBuffer(writerOnly{payload}, io.LimitReader(br, n), buf)
		if err != nil {
			return err
		}
		if w < n {
			return io.ErrUnexpectedEOF
		}
		var crlf [2]byte
		k, err := io.ReadFull(br, crlf[:])
		if _, werr := framing.Write(crlf[:k]); werr != nil {
			return werr
		}
		if err != nil {
			return io.ErrUnexpectedEOF
		}
		if crlf != [2]byte{'\r', '\n'} {
			return errMalformed
		}
	}
	// No trailer: the body ends with an empty line.
	var crlf [2]byte
	k, err := io.ReadFull(br, crlf[:])
	if _, werr := framing.Write(crlf[:k]); werr != nil {
		return werr
	}
	if err != nil {
		return io.ErrUnexpectedEOF
	}
	if crlf != [2]byte{'\r', '\n'} {
		return errMalformed
	}
	return nil
}

// parseChunkSize parses a chunk-size line without its CRLF as net/http does: trailing
// spaces and tabs trimmed, any extension after ';' dropped, 1 to 16 hex digits.
func parseChunkSize(line []byte) (int64, bool) {
	p := bytes.TrimRight(line, " \t")
	if i := bytes.IndexByte(p, ';'); i >= 0 {
		p = p[:i]
	}
	if len(p) == 0 || len(p) > 16 {
		return 0, false
	}
	var n uint64
	for _, c := range p {
		var d byte
		switch {
		case '0' <= c && c <= '9':
			d = c - '0'
		case 'a' <= c && c <= 'f':
			d = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		n = n<<4 | uint64(d)
	}
	if n > 1<<62 {
		return 0, false
	}
	return int64(n), true
}

var errBodyTooLarge = errors.New("transport: body too large")

// limitedBuffer collects at most max bytes.
type limitedBuffer struct {
	b   []byte
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if len(l.b)+len(p) > l.max {
		return 0, errBodyTooLarge
	}
	l.b = append(l.b, p...)
	return len(p), nil
}
