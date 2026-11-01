// Package dockerapi is a small Docker Engine API client over a connection the caller
// dials (on Windows, a local named pipe), and the checks that keep a name from a user or
// an engine within one segment of the API's URL paths.
//
// The client's Transport has no Proxy and its DialContext ignores the address: every
// request goes to the Dial function, and the URL's host is the dummy "docker".
package dockerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// BodyMax bounds every response body that is decoded whole.
const BodyMax = 8 << 20

// Dial opens one connection to the engine.
type Dial func(ctx context.Context) (net.Conn, error)

// Client sends Docker API requests over Dial.
type Client struct {
	c *http.Client
}

// New returns a client that reaches the engine through dial.
func New(dial Dial) *Client {
	return &Client{c: &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: true,
		// A tar stream is already as compact as it gets, and a gzip layer would hide the
		// byte counts the progress shows.
		DisableCompression: true,
	}}}
}

// Close drops idle connections.
func (c *Client) Close() { c.c.CloseIdleConnections() }

// Error is an error answer of the Docker API: its status and the engine's message.
// The message is the engine's text (or a registry's, relayed by it): Error() returns it
// cleaned of control characters and cut to 512 bytes.
type Error struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s %s: Docker API %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		s += ": " + Clean(e.Message, 512)
	}
	return s
}

// StatusOf returns the HTTP status of a Docker API error answer, or 0.
func StatusOf(err error) int {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// IsNotFound reports whether err is the Docker API's 404.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// RequestError is a request that got no answer (the pipe is gone, the context ended):
// Op is the method and path.
type RequestError struct {
	Op  string
	Err error
}

func (e *RequestError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

// Clean makes engine text safe to print on one line: invalid UTF-8 and control
// characters (escape sequences included, and the bidirectional overrides) become
// U+FFFD, and it is cut to at most n bytes on a rune boundary.
func Clean(s string, n int) string { return Clip(CleanText(s, false), n) }

// Do sends one request. The path is built by the caller from validated names only
// (ValidName, ValidImageRef). An answer other than 2xx or 304 becomes an *Error, with its
// body closed; otherwise the caller closes the body.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.c.Do(req)
	if err != nil {
		return nil, &RequestError{Op: method + " " + path, Err: err}
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotModified {
		defer func() { _ = resp.Body.Close() }()
		var m struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(b))
		}
		return nil, &Error{Method: method, Path: path, Status: resp.StatusCode, Message: Clean(m.Message, 1024)}
	}
	return resp, nil
}

// GetJSON GETs path and decodes the answer into out.
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	return c.JSON(ctx, http.MethodGet, path, q, nil, out)
}

// JSON sends a request with in as its JSON body (nil: none) and decodes the answer into
// out (nil: the answer is read and dropped).
func (c *Client) JSON(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var rd io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.Do(ctx, method, path, q, rd, ct)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, BodyMax))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, BodyMax)).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}
