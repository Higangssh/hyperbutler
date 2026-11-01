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
	"strings"
)

// dockerClient talks to the engine's Docker API on this machine's named pipe (the engine
// relays it to the guest's dockerd). Its Transport has no Proxy and its DialContext
// ignores the address: every request goes to dial.
type dockerClient struct {
	c *http.Client
}

// apiBodyMax bounds every Docker API response body that is decoded whole.
const apiBodyMax = 8 << 20

func newDockerClient(dial func(ctx context.Context) (net.Conn, error)) *dockerClient {
	return &dockerClient{c: &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: true,
	}}}
}

func (d *dockerClient) close() { d.c.CloseIdleConnections() }

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

// do sends one request. The path is built by the caller from validated names only
// (validName, validImageRef). A response other than 2xx or 304 becomes an *apiError.
func (d *dockerClient) do(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return nil, fromOther(method+" "+path+": the Docker API request failed", err)
	}
	// 304: a container start or stop found it started or stopped already.
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotModified {
		defer func() { _ = resp.Body.Close() }()
		var m struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(b))
		}
		return nil, &apiError{Status: resp.StatusCode, Message: clip(m.Message, 1024)}
	}
	return resp, nil
}

// getJSON GETs path and decodes the answer into v.
func (d *dockerClient) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	return d.callJSON(ctx, http.MethodGet, path, q, nil, v)
}

// callJSON sends a request and decodes the answer into v (nil: the answer is dropped).
func (d *dockerClient) callJSON(ctx context.Context, method, path string, q url.Values, body, v any) error {
	resp, err := d.do(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if v == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, apiBodyMax))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, apiBodyMax)).Decode(v); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}

// --- the Docker API's types, the fields used -------------------------------------------

type apiPort struct {
	IP          string `json:"IP"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort"`
	Type        string `json:"Type"`
}

type apiContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Ports   []apiPort         `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
}

type apiInspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Tty bool `json:"Tty"`
	} `json:"Config"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
	HostConfig struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

type apiImage struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     int64    `json:"Created"`
	Size        int64    `json:"Size"`
	SharedSize  int64    `json:"SharedSize"`
	Containers  int64    `json:"Containers"`
}

type apiVolume struct {
	Name      string `json:"Name"`
	Driver    string `json:"Driver"`
	Scope     string `json:"Scope"`
	CreatedAt string `json:"CreatedAt"`
	UsageData *struct {
		Size     int64 `json:"Size"`
		RefCount int64 `json:"RefCount"`
	} `json:"UsageData"`
}

type apiDF struct {
	LayersSize int64      `json:"LayersSize"`
	Images     []apiImage `json:"Images"`
	Containers []struct {
		State  string `json:"State"`
		SizeRw int64  `json:"SizeRw"`
	} `json:"Containers"`
	Volumes    []apiVolume `json:"Volumes"`
	BuildCache []struct {
		Size   int64 `json:"Size"`
		InUse  bool  `json:"InUse"`
		Shared bool  `json:"Shared"`
	} `json:"BuildCache"`
}

type apiInfo struct {
	ServerVersion     string `json:"ServerVersion"`
	MemTotal          int64  `json:"MemTotal"`
	NCPU              int    `json:"NCPU"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	Images            int    `json:"Images"`
}

type apiStats struct {
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}
