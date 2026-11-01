package desktopimport

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// cleanupTimeout bounds each removal of a helper container or of a bad copy, which
// runs even after the import's context is canceled.
const cleanupTimeout = 30 * time.Second

// importLabel marks what the import created in hyperbutler (volumes), with the source.
const importLabel = "dev.hyperbutler.import.source"

// copyingLabel marks the hyperbutler helper that fills a new volume, with the volume's
// name. A helper with it left behind means that copy was interrupted (the import was
// killed): the next import removes that volume, which the import created.
const copyingLabel = "dev.hyperbutler.import.copying"

// counter counts the bytes read through it and reports them.
type counter struct {
	r      io.Reader
	n      int64
	report func(n int64)
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.report != nil {
			c.report(c.n)
		}
	}
	return n, err
}

func progressFn(progress func(Event), kind, name, phase string, total int64) func(int64) {
	if progress == nil {
		return nil
	}
	if total < 0 {
		total = 0
	}
	return func(n int64) {
		progress(Event{Kind: EventProgress, Item: kind, Name: name, Phase: phase, Bytes: n, Total: total})
	}
}

// cleanupCtx is a context for cleanup that outlives a canceled import.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// note records a problem found while cleaning up; Run reports them.
func (im *Importer) note(format string, a ...any) {
	im.mu.Lock()
	im.notes = append(im.notes, fmt.Sprintf(format, a...))
	im.mu.Unlock()
}

func (im *Importer) removeSrcHelper(ctx context.Context, id string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := im.src.RemoveHelper(cctx, id); err != nil {
		im.note("could not remove the helper container %s in Docker Desktop (%v); remove it with: docker --context desktop-linux rm %s", shortID(id), err, id)
	}
}

func (im *Importer) createDstHelper(ctx context.Context, image, volume string, copying bool) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	q := url.Values{"name": {source.HelperName()}}
	body := source.HelperBody(image, volume, false)
	if copying {
		body["Labels"].(map[string]string)[copyingLabel] = volume
	}
	if err := im.dst.JSON(ctx, http.MethodPost, "/containers/create", q, body, &created); err != nil {
		return "", fmt.Errorf("create the helper container in hyperbutler: %w", err)
	}
	if err := dockerapi.ValidName("container id", created.ID); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (im *Importer) removeDstHelper(ctx context.Context, id string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	err := im.dst.JSON(cctx, http.MethodDelete, "/containers/"+id, url.Values{"v": {"0"}, "force": {"1"}}, nil, nil)
	if err != nil && !dockerapi.IsNotFound(err) {
		im.note("could not remove the helper container %s in hyperbutler (%v); remove it with: docker --context hyperbutler rm %s", shortID(id), err, id)
	}
}

// hashSource hashes a Docker Desktop volume through a helper container.
func (im *Importer) hashSource(ctx context.Context, image, volume string, total int64, phase string, progress func(Event)) (*tarSum, error) {
	id, err := im.src.CreateHelper(ctx, image, volume)
	if err != nil {
		return nil, fmt.Errorf("Docker Desktop's volume %s: %w", volume, err) //nolint:staticcheck // ST1005: a product name
	}
	defer im.removeSrcHelper(ctx, id)
	resp, err := im.src.Get(ctx, "/containers/"+id+"/archive", url.Values{"path": {source.HelperMount}})
	if err != nil {
		return nil, fmt.Errorf("read Docker Desktop's volume %s: %w", volume, err)
	}
	defer func() { _ = resp.Body.Close() }()
	s, err := hashArchive(&counter{r: resp.Body, report: progressFn(progress, KindVolume, volume, phase, total)}, nil)
	if err != nil {
		return nil, fmt.Errorf("Docker Desktop's volume %s: %w", volume, err) //nolint:staticcheck // ST1005: a product name
	}
	return s, nil
}

// hashTarget hashes a hyperbutler volume through a helper container.
func (im *Importer) hashTarget(ctx context.Context, image, volume, phase string, progress func(Event)) (*tarSum, error) {
	id, err := im.createDstHelper(ctx, image, volume, false)
	if err != nil {
		return nil, err
	}
	defer im.removeDstHelper(ctx, id)
	return im.hashDstHelper(ctx, id, volume, phase, progress)
}

func (im *Importer) hashDstHelper(ctx context.Context, id, volume, phase string, progress func(Event)) (*tarSum, error) {
	resp, err := im.dst.Do(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {source.HelperMount}}, nil, "")
	if err != nil {
		return nil, fmt.Errorf("read hyperbutler's volume %s: %w", volume, err)
	}
	defer func() { _ = resp.Body.Close() }()
	s, err := hashArchive(&counter{r: resp.Body, report: progressFn(progress, KindVolume, volume, phase, 0)}, nil)
	if err != nil {
		return nil, fmt.Errorf("hyperbutler's volume %s: %w", volume, err)
	}
	return s, nil
}

// copyArchive streams the source helper's volume archive into the target helper: one
// pass reads it from Docker Desktop, checks and hashes every entry (hashArchive) and
// writes it to hyperbutler's archive API, which extracts it inside the VM. Nothing is
// buffered beyond the pipe, and nothing is extracted on this machine.
func (im *Importer) copyArchive(ctx context.Context, srcHelper, dstHelper, volume string, total int64, progress func(Event)) (*tarSum, int64, error) {
	resp, err := im.src.Get(ctx, "/containers/"+srcHelper+"/archive", url.Values{"path": {source.HelperMount}})
	if err != nil {
		return nil, 0, fmt.Errorf("read Docker Desktop's volume %s: %w", volume, err)
	}
	defer func() { _ = resp.Body.Close() }()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		r, err := im.dst.Do(ctx, http.MethodPut, "/containers/"+dstHelper+"/archive", url.Values{"path": {"/"}}, pr, "application/x-tar")
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
			_ = r.Body.Close()
		}
		_ = pr.CloseWithError(errors.New("hyperbutler's archive API stopped reading"))
		done <- err
	}()
	c := &counter{r: resp.Body, report: progressFn(progress, KindVolume, volume, PhaseCopy, total)}
	tw := tar.NewWriter(pw)
	sum, herr := hashArchive(c, tw)
	if herr == nil {
		herr = tw.Close()
	}
	_ = pw.CloseWithError(herr) // nil: the upload ends normally
	perr := <-done
	switch {
	case perr != nil:
		return nil, c.n, fmt.Errorf("write hyperbutler's volume %s: %w", volume, perr)
	case herr != nil:
		return nil, c.n, herr
	}
	return sum, c.n, nil
}

// removeDstVolume removes a volume the import created in hyperbutler (a failed or
// canceled copy).
func (im *Importer) removeDstVolume(ctx context.Context, name string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	err := im.dst.JSON(cctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil)
	if err != nil && !dockerapi.IsNotFound(err) {
		im.note("could not remove the incomplete copy of volume %s in hyperbutler (%v); remove it with: docker --context hyperbutler volume rm %s", name, err, name)
	}
}
