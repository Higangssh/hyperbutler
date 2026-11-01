package desktopimport

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// Each such volume also has source.RunLabel, the id of the run that created it.
const importLabel = "dev.hyperbutler.import.source"

// copyingLabel marks the hyperbutler helper that fills a new volume, with the volume's
// name. The helper is removed right after the copy, before the copy is verified: one
// left behind means that copy was interrupted (the import was killed), and the next
// import removes that volume if the helper's run created it (findStale).
const copyingLabel = "dev.hyperbutler.import.copying"

// tempForLabel marks the temporary volume --overwrite-volumes fills first, with the name
// of the volume it is for.
const tempForLabel = "dev.hyperbutler.import.temp-for"

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
		im.note("could not remove the helper container %s in Docker Desktop (%v); remove it with: docker --context desktop-linux rm -v %s", shortID(id), err, id)
	}
}

// createDstHelper creates a helper in hyperbutler that mounts volume: read-only to read
// it, or writable and labeled copyingLabel to fill it.
func (im *Importer) createDstHelper(ctx context.Context, run, image, volume string, copying bool) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	q := url.Values{"name": {source.HelperName()}}
	var labels map[string]string
	if copying {
		labels = map[string]string{copyingLabel: volume}
	}
	body := source.HelperBody(image, volume, run, !copying, labels)
	if err := im.dst.JSON(ctx, http.MethodPost, "/containers/create", q, body, &created); err != nil {
		return "", fmt.Errorf("create the helper container in hyperbutler: %w", err)
	}
	if err := dockerapi.ValidName("container id", created.ID); err != nil {
		return "", err
	}
	return created.ID, nil
}

// removeDstHelper removes a helper this import created in hyperbutler, with any
// anonymous volume (v=1), and notes a failure.
func (im *Importer) removeDstHelper(ctx context.Context, id string) {
	if err := im.deleteDstHelper(ctx, id); err != nil {
		im.note("could not remove the helper container %s in hyperbutler (%v); remove it with: docker --context hyperbutler rm -v %s", shortID(id), err, id)
	}
}

func (im *Importer) deleteDstHelper(ctx context.Context, id string) error {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	err := im.dst.JSON(cctx, http.MethodDelete, "/containers/"+id, url.Values{"v": {"1"}, "force": {"1"}}, nil, nil)
	if dockerapi.IsNotFound(err) {
		return nil
	}
	return err
}

// removeRetries are the waits between tries to remove the helper that filled a volume:
// while it is there, a killed import's recovery would take the volume for a half copy.
var removeRetries = []time.Duration{200 * time.Millisecond, time.Second}

func (im *Importer) removeCopyingHelper(ctx context.Context, id string) error {
	err := im.deleteDstHelper(ctx, id)
	for _, d := range removeRetries {
		if err == nil {
			return nil
		}
		t := time.NewTimer(d)
		<-t.C
		err = im.deleteDstHelper(ctx, id)
	}
	return err
}

// hashSource hashes a Docker Desktop volume through a helper container.
func (im *Importer) hashSource(ctx context.Context, run, image, volume string, total int64, phase string, progress func(Event)) (*tarSum, error) {
	id, err := im.src.CreateHelper(ctx, image, volume, run)
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

// hashTarget hashes a hyperbutler volume through a helper container that mounts it
// read-only.
func (im *Importer) hashTarget(ctx context.Context, run, image, volume, phase string, progress func(Event)) (*tarSum, error) {
	id, err := im.createDstHelper(ctx, run, image, volume, false)
	if err != nil {
		return nil, err
	}
	defer im.removeDstHelper(ctx, id)
	resp, err := im.dstArchive(ctx, id)
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

func (im *Importer) dstArchive(ctx context.Context, helper string) (*http.Response, error) {
	return im.dst.Do(ctx, http.MethodGet, "/containers/"+helper+"/archive", url.Values{"path": {source.HelperMount}}, nil, "")
}

// copyArchive streams a volume archive (open's answer) into the target helper: one pass
// reads it, checks and hashes every entry (hashArchive) and writes it to hyperbutler's
// archive API, which extracts it inside the VM. Nothing is buffered beyond the pipe,
// and nothing is extracted on this machine.
func (im *Importer) copyArchive(ctx context.Context, open func(context.Context) (*http.Response, error), dstHelper, volume string, total int64, progress func(Event)) (*tarSum, int64, error) {
	resp, err := open(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("read the volume %s: %w", volume, err)
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

// fill streams open's volume archive into hyperbutler's volume target through a new
// helper labeled copyingLabel, and removes that helper before it returns (trying again
// on an error: an error to remove it fails the copy, which then removes the volume).
func (im *Importer) fill(ctx context.Context, st *runState, img, target string, open func(context.Context) (*http.Response, error), item string, total int64, progress func(Event)) (*tarSum, int64, error) {
	dh, err := im.createDstHelper(ctx, st.run.id, img, target, true)
	if err != nil {
		return nil, 0, err
	}
	sum, n, err := im.copyArchive(ctx, open, dh, item, total, progress)
	if err == nil && im.hooks.keepCopyingHelper {
		return sum, n, nil
	}
	if rerr := im.removeCopyingHelper(ctx, dh); rerr != nil {
		if err != nil {
			im.note("could not remove the helper container %s in hyperbutler (%v); remove it with: docker --context hyperbutler rm -v %s", shortID(dh), rerr, dh)
		} else {
			err = fmt.Errorf("could not remove the helper container that filled it in hyperbutler (%w)", rerr)
		}
	}
	return sum, n, err
}

// verify hashes hyperbutler's volume afresh, through a new read-only helper, and wants.
func (im *Importer) verify(ctx context.Context, st *runState, img, volume, want string, progress func(Event)) error {
	got, err := im.hashTarget(ctx, st.run.id, img, volume, PhaseVerify, progress)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if got.Sum() != want {
		return fmt.Errorf("verification failed: the copy in hyperbutler hashes %s, Docker Desktop's volume %s", got.Sum(), want)
	}
	return nil
}

// fillVolume copies the Docker Desktop volume vp into hyperbutler's (new, empty) volume
// target and verifies it: the stream's hash must equal the one recorded before the
// import (unless containers write it), and a fresh export of the copy must hash the
// same. The helpers are removed before it returns.
func (im *Importer) fillVolume(ctx context.Context, st *runState, img string, vp VolumePlan, target, before string, live bool, progress func(Event)) (string, int64, error) {
	sh, err := im.src.CreateHelper(ctx, st.plan.HelperImage, vp.Name, st.run.id)
	if err != nil {
		return "", 0, fmt.Errorf("Docker Desktop's helper container: %w", err) //nolint:staticcheck // ST1005: a product name
	}
	defer im.removeSrcHelper(ctx, sh)
	open := func(ctx context.Context) (*http.Response, error) {
		return im.src.Get(ctx, "/containers/"+sh+"/archive", url.Values{"path": {source.HelperMount}})
	}
	sum, n, err := im.fill(ctx, st, img, target, open, vp.Name, vp.Size, progress)
	if err != nil {
		return "", n, err
	}
	if !live && sum.Sum() != before {
		return "", n, errors.New("Docker Desktop's volume changed during the copy (is a container writing to it?)") //nolint:staticcheck // ST1005: a product name
	}
	if err := im.verify(ctx, st, img, target, sum.Sum(), progress); err != nil {
		return "", n, err
	}
	return sum.Sum(), n, nil
}

// refill copies hyperbutler's volume from (read-only) into its new volume to, and
// verifies the copy hashes want.
func (im *Importer) refill(ctx context.Context, st *runState, img, from, to, want string, total int64, progress func(Event)) error {
	rh, err := im.createDstHelper(ctx, st.run.id, img, from, false)
	if err != nil {
		return err
	}
	defer im.removeDstHelper(ctx, rh)
	open := func(ctx context.Context) (*http.Response, error) { return im.dstArchive(ctx, rh) }
	sum, _, err := im.fill(ctx, st, img, to, open, to, total, progress)
	if err != nil {
		return err
	}
	if sum.Sum() != want {
		return fmt.Errorf("verification failed: the copy of %s hashes %s, not %s", from, sum.Sum(), want)
	}
	return im.verify(ctx, st, img, to, want, progress)
}

// removeDstVolume removes a volume the import created in hyperbutler (a failed or
// canceled copy) and reports whether it is gone.
func (im *Importer) removeDstVolume(ctx context.Context, name string) bool {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	err := im.dst.JSON(cctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil)
	if err != nil && !dockerapi.IsNotFound(err) {
		im.note("could not remove the incomplete copy of volume %s in hyperbutler (%v); remove it with: docker --context hyperbutler volume rm %s", name, err, name)
		return false
	}
	return true
}

// tempVolumeName is the temporary volume --overwrite-volumes fills for volume name.
func tempVolumeName(run, name string) string {
	h := sha256.Sum256([]byte(name))
	return "hyperbutler-import-" + run + "-" + hex.EncodeToString(h[:6])
}
