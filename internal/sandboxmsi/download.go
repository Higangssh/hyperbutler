package sandboxmsi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

const (
	// maxRedirects bounds a download's redirects. GitHub sends one, to its asset host.
	maxRedirects = 5
	// headerTimeout bounds the connection and the answer's headers.
	headerTimeout = 30 * time.Second
	// DefaultIdle is how long a download may receive nothing before it gives up.
	DefaultIdle = 60 * time.Second
)

// NewClient is the download's HTTP client: https only (a redirect elsewhere is refused), TLS
// 1.2 or later, no cookies, and no overall timeout, because a slow line may take minutes; a
// stall ends it instead (Download). proxy nil is none, roots nil the system's.
func NewClient(proxy func(*http.Request) (*url.URL, error), roots *x509.CertPool) *http.Client {
	if roots == nil {
		if p, err := x509.SystemCertPool(); err == nil {
			roots = p
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 proxy,
			TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DisableCompression:    true,
			TLSHandshakeTimeout:   headerTimeout,
			ResponseHeaderTimeout: headerTimeout,
			MaxIdleConns:          1,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			// Another host is fine, GitHub's assets are on one and the hash decides; plain
			// http is not: there is no reason to tell the path to the whole network.
			if req.URL.Scheme != "https" {
				return errors.New("redirect to a URL that is not https refused")
			}
			return nil
		},
	}
}

// Download is a Fetch from address, which must be https: it creates path and writes at most
// max bytes to it, refusing a larger answer before it starts when the server says so.
// userAgent is the only header it adds. progress, if any, gets the bytes so far and the
// total; idle (0: DefaultIdle) ends a download that receives nothing for that long.
func Download(c *http.Client, address, userAgent string, idle time.Duration, progress func(done, total int64)) func(ctx context.Context, path string, max int64) error {
	return func(ctx context.Context, path string, max int64) (err error) {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		if u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("%s is not an https URL", address)
		}
		if idle <= 0 {
			idle = DefaultIdle
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		req.Header = http.Header{"User-Agent": {userAgent}}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if t, ok := c.Transport.(*http.Transport); ok {
			defer t.CloseIdleConnections()
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the server answered %d for %s", resp.StatusCode, address)
		}
		if resp.ContentLength > max {
			return fmt.Errorf("%w: the server offers %d bytes, at most %d are expected", ErrTooLarge, resp.ContentLength, max)
		}
		out, err := createNew(path)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}()
		var stalled atomic.Bool
		timer := time.AfterFunc(idle, func() {
			stalled.Store(true)
			cancel()
		})
		defer timer.Stop()
		err = copyBounded(ctx, out, resp.Body, max, func(done int64) {
			timer.Reset(idle)
			if progress != nil {
				progress(done, max)
			}
		})
		if err != nil && stalled.Load() {
			return fmt.Errorf("the download stalled: nothing arrived for %v", idle)
		}
		return err
	}
}
