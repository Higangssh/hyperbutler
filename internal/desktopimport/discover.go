package desktopimport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// DesktopContext is the docker CLI context Docker Desktop registers for its Linux
// engine, and DesktopPipe the pipe it serves when the context gives none.
const (
	DesktopContext = "desktop-linux"
	DesktopPipe    = `\\.\pipe\dockerDesktopLinuxEngine`
)

// How Discover found the endpoint (Endpoint.Via).
const (
	ViaContext  = "context"  // the desktop-linux context's endpoint
	ViaDefault  = "default"  // DesktopPipe
	ViaOverride = "override" // Options.SourcePipe
)

var (
	// ErrDesktopNotRunning means nothing answers on Docker Desktop's engine pipe.
	ErrDesktopNotRunning = errors.New("Docker Desktop is not running: start Docker Desktop first") //nolint:staticcheck // ST1005: a product name starts the sentence
	// ErrTargetNotRunning means hyperbutler's Docker API does not answer.
	ErrTargetNotRunning = errors.New("hyperbutler's engine is not running: start it with `hyperbutler start`")
	// ErrSameEngine means the source and the target are one engine.
	ErrSameEngine = errors.New("the Docker Desktop endpoint reaches hyperbutler's own engine: there is nothing to import")
)

// Options say where the two engines are.
type Options struct {
	// Dial opens a connection to a named pipe on this machine (winio.DialPipeContext).
	Dial func(ctx context.Context, pipe string) (net.Conn, error)
	// DockerConfig is the docker CLI's config directory; empty: %DOCKER_CONFIG% or
	// ~/.docker. Only the desktop-linux context's meta.json is read from it.
	DockerConfig string
	// SourcePipe, when set, is used instead of discovering Docker Desktop's pipe.
	SourcePipe string
	// TargetPipe is hyperbutler's Docker API pipe (Open only).
	TargetPipe string
}

// Endpoint is Docker Desktop's engine as Discover found it.
type Endpoint struct {
	Pipe string `json:"pipe"`
	Via  string `json:"via"` // ViaContext, ViaDefault or ViaOverride
	// Version is the engine's version (GET /version), Note anything worth saying about
	// how it was found (a context that was not a local pipe).
	Version string `json:"version,omitempty"`
	Note    string `json:"note,omitempty"`
}

// pingTimeout bounds each engine's first answer.
const pingTimeout = 10 * time.Second

// pipeFromHost turns a docker endpoint URL (npipe:////./pipe/NAME) into a pipe name.
// Anything but a local named pipe is refused.
func pipeFromHost(host string) (string, error) {
	rest, ok := strings.CutPrefix(host, "npipe://")
	if !ok {
		return "", fmt.Errorf("endpoint %q is not a named pipe on this machine", dockerapi.Clean(host, 120))
	}
	p := strings.ReplaceAll(rest, "/", `\`)
	if !strings.HasPrefix(p, `\\.\pipe\`) {
		return "", fmt.Errorf("endpoint %q is not a named pipe on this machine", dockerapi.Clean(host, 120))
	}
	if err := transport.ValidatePipeName(p); err != nil {
		return "", err
	}
	return p, nil
}

// findEndpoint picks the pipe: Options.SourcePipe, the desktop-linux context's
// endpoint, or DesktopPipe.
func findEndpoint(o Options) (Endpoint, error) {
	if o.SourcePipe != "" {
		if err := transport.ValidatePipeName(o.SourcePipe); err != nil {
			return Endpoint{}, err
		}
		return Endpoint{Pipe: o.SourcePipe, Via: ViaOverride}, nil
	}
	st := dockerctx.Store{Dir: o.DockerConfig}
	if st.Dir == "" {
		var err error
		if st, err = dockerctx.DefaultStore(); err != nil {
			return Endpoint{Pipe: DesktopPipe, Via: ViaDefault}, nil //nolint:nilerr // no home directory: the default pipe
		}
	}
	c, err := st.Get(DesktopContext)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Endpoint{Pipe: DesktopPipe, Via: ViaDefault}, nil
	case err != nil:
		return Endpoint{Pipe: DesktopPipe, Via: ViaDefault, Note: "the desktop-linux context cannot be read (" + err.Error() + "); using the default pipe"}, nil
	case c.Host == "":
		return Endpoint{Pipe: DesktopPipe, Via: ViaDefault}, nil
	}
	p, err := pipeFromHost(c.Host)
	if err != nil {
		return Endpoint{Pipe: DesktopPipe, Via: ViaDefault, Note: "the desktop-linux context's " + err.Error() + "; using the default pipe"}, nil
	}
	return Endpoint{Pipe: p, Via: ViaContext}, nil
}

func dialer(o Options, pipe string) dockerapi.Dial {
	return func(ctx context.Context) (net.Conn, error) { return o.Dial(ctx, pipe) }
}

// Discover finds Docker Desktop's engine and checks that it answers. It returns an
// error wrapping ErrDesktopNotRunning when it does not.
func Discover(ctx context.Context, o Options) (Endpoint, error) {
	ep, err := findEndpoint(o)
	if err != nil {
		return ep, err
	}
	c := dockerapi.New(dialer(o, ep.Pipe))
	defer c.Close()
	src := source.New(c)
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	var v struct {
		Version string `json:"Version"`
	}
	if err := src.GetJSON(ctx, "/version", nil, &v); err != nil {
		if dockerapi.StatusOf(err) == 0 {
			return ep, fmt.Errorf("%w (nothing answers on %s)", ErrDesktopNotRunning, ep.Pipe)
		}
		return ep, fmt.Errorf("Docker Desktop's engine on %s: %w", ep.Pipe, err) //nolint:staticcheck // ST1005: a product name
	}
	ep.Version = dockerapi.Clean(v.Version, 64)
	return ep, nil
}

// engineInfo is the part of GET /info the importer uses.
type engineInfo struct {
	ID           string      `json:"ID"`
	Name         string      `json:"Name"`
	DriverStatus [][2]string `json:"DriverStatus"`
}

// containerdStore reports whether the engine keeps images in containerd's store (image
// ids are then manifest or index digests, not config digests).
func (i engineInfo) containerdStore() bool {
	for _, kv := range i.DriverStatus {
		if kv[0] == "driver-type" && strings.Contains(kv[1], "containerd.snapshotter") {
			return true
		}
	}
	return false
}
