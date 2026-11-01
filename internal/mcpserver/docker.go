package mcpserver

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

	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// dockerClient is internal/dockerapi's client with this server's errors: a transport
// failure is labeled with fromOther, and an API error keeps dockerd's message for
// untrustedText.
type dockerClient struct {
	c *dockerapi.Client
}

func newDockerClient(dial func(ctx context.Context) (net.Conn, error)) *dockerClient {
	return &dockerClient{c: dockerapi.New(dial)}
}

func (d *dockerClient) close() { d.c.Close() }

// apiError is an error answer of the Docker API: its status and dockerd's message.
// Error() is the status only: the message is dockerd's (or a registry's, relayed by it),
// so errorText returns it in untrustedText.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("Docker API: %d %s", e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		s += " (dockerd's message is in untrustedText)"
	}
	return s
}

// isNotFound reports whether err is the Docker API's 404.
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// wrap turns dockerapi's errors into this server's.
func wrap(err error) error {
	var ae *dockerapi.Error
	var re *dockerapi.RequestError
	switch {
	case errors.As(err, &ae):
		return &apiError{Status: ae.Status, Message: ae.Message}
	case errors.As(err, &re):
		return fromOther(re.Op+": the Docker API request failed", re.Err)
	}
	return err
}

// do sends one request. The path is built by the caller from validated names only
// (validName, validImageRef). A response other than 2xx or 304 becomes an *apiError.
func (d *dockerClient) do(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := d.c.Do(ctx, method, path, q, rd, ct)
	return resp, wrap(err)
}

// getJSON GETs path and decodes the answer into v.
func (d *dockerClient) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	return wrap(d.c.GetJSON(ctx, path, q, v))
}

// callJSON sends a request and decodes the answer into v (nil: the answer is dropped).
func (d *dockerClient) callJSON(ctx context.Context, method, path string, q url.Values, body, v any) error {
	return wrap(d.c.JSON(ctx, method, path, q, body, v))
}

// The Docker API's types, the fields used (internal/dockerapi).
type (
	apiPort      = dockerapi.Port
	apiContainer = dockerapi.Container
	apiInspect   = dockerapi.Inspect
	apiImage     = dockerapi.Image
	apiVolume    = dockerapi.Volume
	apiDF        = dockerapi.DF
	apiInfo      = dockerapi.Info
	apiStats     = dockerapi.Stats
)
