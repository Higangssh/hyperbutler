//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/Higangssh/hyperbutler/internal/desktopimport"
	"github.com/Higangssh/hyperbutler/internal/mcpserver"
	"github.com/Higangssh/hyperbutler/internal/redact"
)

// cmdImport is `hyperbutler import --from-desktop` (REQ-UX-023): copy images and
// volumes from Docker Desktop's running engine into hyperbutler, through the Docker API
// of both. The work is internal/desktopimport's; this is its command line.
func cmdImport(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("import", stderr)
	fromDesktop := fs.Bool("from-desktop", false, "import from Docker Desktop's engine (it must be running)")
	images := fs.String("image", "", "images to import, comma-separated (tags or ids)")
	allImages := fs.Bool("all-images", false, "import every tagged image")
	volumes := fs.String("volume", "", "volumes to import, comma-separated")
	allVolumes := fs.Bool("all-volumes", false, "import every local volume")
	overwrite := fs.Bool("overwrite-volumes", false, "replace a hyperbutler volume of the same name whose content differs")
	allowLive := fs.Bool("allow-live-volumes", false, "copy volumes that running Docker Desktop containers mount (the copy may be inconsistent)")
	forceSpace := fs.Bool("force-space", false, "start the copy even when it does not look like it fits in the VM's data disk or on the host volume")
	dryRun := fs.Bool("dry-run", false, "only list what there is and what would be copied")
	asJSON := fs.Bool("json", false, "print JSON (schema "+fmt.Sprint(SchemaVersion)+")")
	// For tests: another engine's pipe stands in for Docker Desktop's.
	fromPipe := fs.String("from-pipe", "", "")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage of %s:\n", fs.Name())
		fs.VisitAll(func(f *flag.Flag) {
			if f.Name != "from-pipe" {
				_, _ = fmt.Fprintf(fs.Output(), "  --%s\n    \t%s\n", f.Name, f.Usage)
			}
		})
	}
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if !*fromDesktop && *fromPipe == "" {
		return usagef("import: say where from: --from-desktop (the only source so far)")
	}
	sel := desktopimport.Selection{
		Images: splitList(*images), AllImages: *allImages, Volumes: splitList(*volumes), AllVolumes: *allVolumes,
		OverwriteVolumes: *overwrite, AllowLiveVolumes: *allowLive,
	}
	picked := len(sel.Images) > 0 || sel.AllImages || len(sel.Volumes) > 0 || sel.AllVolumes
	interactive := !picked && !*dryRun && !*asJSON && isConsole(stdin)
	if !picked && !*dryRun && !interactive {
		return usagef("import: say what to import: --image, --all-images, --volume or --all-volumes (--dry-run lists what there is)")
	}

	in, err := openInstall()
	if err != nil {
		return err
	}
	// A dry run takes it too: its plan removes left-over helpers, and compares volumes
	// with helpers of its own.
	lock, err := in.importLock()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSignals := cancelOnInterrupt(cancel, stderr)
	defer stopSignals()

	target, err := (&mcpHost{}).Pipe(ctx)
	if errors.Is(err, mcpserver.ErrEngineStopped) {
		return errors.New("hyperbutler is not running: start it with `hyperbutler start`, then import")
	}
	if err != nil {
		return err
	}
	im, err := desktopimport.Open(ctx, desktopimport.Options{
		Dial:       func(ctx context.Context, pipe string) (net.Conn, error) { return winio.DialPipeContext(ctx, pipe) },
		SourcePipe: *fromPipe,
		TargetPipe: target,
	})
	if err != nil {
		return errors.New(redact.String(err.Error()))
	}
	defer im.Close()

	pr := newImportProgress(stderr, *asJSON)
	plan, err := im.Plan(ctx, desktopimport.PlanOptions{CompareVolumes: *dryRun || interactive, Progress: pr.event})
	pr.clear()
	if err != nil {
		return errors.New(redact.String(err.Error()))
	}
	out := ImportJSON{Schema: SchemaVersion, DryRun: *dryRun, Target: target, Notes: plan.Notes,
		Source: ImportSourceJSON{Pipe: plan.Source.Pipe, Via: plan.Source.Via, Version: plan.Source.Version}}
	if *dryRun {
		if picked {
			plan = narrowPlan(plan, sel)
		}
		for _, ip := range plan.Images {
			out.Items = append(out.Items, imagePlanJSON(ip))
		}
		for _, vp := range plan.Volumes {
			out.Items = append(out.Items, volumePlanJSON(vp))
		}
		out.OK = true
		if *asJSON {
			return writeJSON(stdout, out)
		}
		writePlan(stdout, plan, false)
		_, _ = fmt.Fprintln(stdout, "Dry run: nothing was copied. Containers are not moved: create them again in hyperbutler with docker compose up or docker run.")
		return nil
	}
	if interactive {
		writePlan(stdout, plan, true)
		if sel, err = askSelection(stdin, stdout, plan, sel); err != nil {
			return err
		}
		if len(sel.Images) == 0 && len(sel.Volumes) == 0 {
			_, _ = fmt.Fprintln(stdout, "Nothing picked; nothing was copied.")
			return nil
		}
	}

	space, err := in.importSpaceFor(plan, sel)
	if err != nil {
		return err
	}
	if err := space.check(); err != nil && !*forceSpace {
		return err
	} else if err != nil {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: warning: %v (going on: --force-space)\n", err)
	}
	if space.unknown > 0 && !*asJSON {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: note: Docker Desktop gave no size for %s, which the space check does not count\n", countText(space.unknown, "volume"))
	}

	res, err := im.Run(ctx, plan, sel, pr.event)
	pr.clear()
	if err != nil {
		return errors.New(redact.String(err.Error()))
	}
	status := map[string]desktopimport.Status{}
	for _, ip := range plan.Images {
		status["image\x00"+ip.ID] = ip.Status
	}
	for _, vp := range plan.Volumes {
		status["volume\x00"+vp.Name] = vp.Status
	}
	for _, it := range res.Items {
		out.Items = append(out.Items, itemJSON(it, status[it.Kind+"\x00"+itemKey(it)]))
	}
	for _, it := range res.Skipped {
		out.Skipped = append(out.Skipped, itemJSON(it, desktopimport.StatusUnsupported))
	}
	out.BytesCopied, out.Canceled, out.OK = res.Bytes, res.Canceled, res.OK()
	out.SourceCheck = &res.Source
	out.Notes = append(out.Notes, res.Notes...)
	for i := range out.Notes {
		out.Notes[i] = redact.String(out.Notes[i])
	}
	if *asJSON {
		if err := writeJSON(stdout, out); err != nil {
			return err
		}
	} else {
		writeResult(stdout, res, out.Notes)
	}
	if !res.OK() {
		return &shownError{errors.New("import: not everything was imported")}
	}
	return nil
}

func itemKey(it desktopimport.ItemResult) string {
	if it.Kind == desktopimport.KindImage {
		return it.ID
	}
	return it.Name
}

// cancelOnInterrupt cancels on the first Ctrl+C (the import undoes the item in progress,
// removes its helpers and checks Docker Desktop) and exits on the second.
func cancelOnInterrupt(cancel context.CancelFunc, stderr io.Writer) (stop func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
	done := make(chan struct{})
	go func() {
		n := 0
		for {
			select {
			case <-ch:
				n++
				if n == 1 {
					_, _ = fmt.Fprintln(stderr, "\nhyperbutler: import: canceling; removing what was half copied and checking Docker Desktop (Ctrl+C again to quit at once)")
					cancel()
					continue
				}
				_, _ = fmt.Fprintln(stderr, "\nhyperbutler: import: quit; the next import removes any helper container left behind")
				os.Exit(130)
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// narrowPlan keeps the plan's items a selection names (for a dry run with names).
func narrowPlan(p *desktopimport.Plan, sel desktopimport.Selection) *desktopimport.Plan {
	n := *p
	if !sel.AllImages {
		n.Images = nil
		for _, ip := range p.Images {
			for _, name := range sel.Images {
				if ip.ID == name || strings.TrimPrefix(ip.ID, "sha256:") == name || containsRef(ip.Tags, name) {
					n.Images = append(n.Images, ip)
					break
				}
			}
		}
	}
	if !sel.AllVolumes {
		n.Volumes = nil
		for _, vp := range p.Volumes {
			for _, name := range sel.Volumes {
				if vp.Name == name {
					n.Volumes = append(n.Volumes, vp)
				}
			}
		}
	}
	return &n
}

func containsRef(tags []string, name string) bool {
	for _, t := range tags {
		if t == name || t == name+":latest" {
			return true
		}
	}
	return false
}

func imagePlanJSON(ip desktopimport.ImagePlan) ImportItemJSON {
	return ImportItemJSON{Kind: desktopimport.KindImage, Name: ip.Name(), ID: ip.ID, Tags: ip.Tags, SizeBytes: ip.Size,
		Status: string(ip.Status), Reason: ip.Reason, MissingTags: ip.MissingTags}
}

func volumePlanJSON(vp desktopimport.VolumePlan) ImportItemJSON {
	return ImportItemJSON{Kind: desktopimport.KindVolume, Name: vp.Name, SizeBytes: vp.Size, Status: string(vp.Status),
		Reason: vp.Reason, UsedBy: vp.UsedBy, Hash: vp.SourceHash}
}

func itemJSON(it desktopimport.ItemResult, st desktopimport.Status) ImportItemJSON {
	return ImportItemJSON{Kind: it.Kind, Name: it.Name, ID: it.ID, Tags: it.Tags, SizeBytes: it.Size, Status: string(st),
		Outcome: string(it.Outcome), BytesCopied: it.Bytes, Reason: redact.String(it.Reason), AddedTags: it.AddedTags, Hash: it.Hash}
}

func sizeText(n int64) string {
	if n < 0 {
		return "?"
	}
	return humanSize(uint64(n))
}

// humanSize is a byte count with one decimal in the largest unit that fits:
// 12.3 MiB, 300.0 MiB, 17 B.
func humanSize(n uint64) string {
	if n < 1024 {
		return strconv.FormatUint(n, 10) + " B"
	}
	f, units := float64(n), []string{"KiB", "MiB", "GiB", "TiB"}
	i := -1
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + units[i]
}

func statusText(s desktopimport.Status) string {
	switch s {
	case desktopimport.StatusNew:
		return "new"
	case desktopimport.StatusAlready:
		return "already imported"
	case desktopimport.StatusPartial:
		return "partly present (tags)"
	case desktopimport.StatusConflict:
		return "conflict"
	case desktopimport.StatusExists:
		return "name exists in hyperbutler"
	}
	return "cannot import"
}

// writePlan prints the plan: numbered when the user picks from it.
func writePlan(w io.Writer, p *desktopimport.Plan, numbered bool) {
	_, _ = fmt.Fprintf(w, "Docker Desktop's engine: %s (%s)\n", p.Source.Pipe, p.Source.Version)
	cols := func(i int, c ...string) []string {
		switch {
		case numbered && i < 0:
			return append([]string{"#"}, c...)
		case numbered:
			return append([]string{strconv.Itoa(i+1) + "."}, c...)
		}
		return c
	}
	tw := newTable(w, cols(-1, "IMAGE", "ID", "SIZE", "STATUS", "NOTE")...)
	_, _ = fmt.Fprintf(w, "\nImages (%d):\n", len(p.Images))
	for i, ip := range p.Images {
		tw.row(cols(i, ip.Name(), shortImageID(ip.ID), sizeText(ip.Size), statusText(ip.Status), ip.Reason)...)
	}
	tw.flush()
	_, _ = fmt.Fprintf(w, "\nVolumes (%d):\n", len(p.Volumes))
	tw = newTable(w, cols(-1, "VOLUME", "SIZE", "STATUS", "NOTE")...)
	for i, vp := range p.Volumes {
		why := vp.Reason
		if len(vp.UsedBy) > 0 {
			why = strings.TrimPrefix(why+"; ", "; ") + "in use by running containers in Docker Desktop: " + strings.Join(vp.UsedBy, ", ") + " (stop them there first)"
		}
		tw.row(cols(i, vp.Name, sizeText(vp.Size), statusText(vp.Status), why)...)
	}
	tw.flush()
	for _, n := range p.Notes {
		_, _ = fmt.Fprintln(w, "Note:", redact.String(n))
	}
	_, _ = fmt.Fprintln(w)
}

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// askSelection asks which numbered images and volumes to import.
func askSelection(stdin io.Reader, w io.Writer, p *desktopimport.Plan, sel desktopimport.Selection) (desktopimport.Selection, error) {
	in := bufio.NewReader(stdin)
	ask := func(what string, n int) ([]int, error) {
		if n == 0 {
			return nil, nil
		}
		for {
			_, _ = fmt.Fprintf(w, "%s to import (numbers such as 1,3-4; all; or Enter for none): ", what)
			line, err := in.ReadString('\n')
			if err != nil && line == "" {
				return nil, fmt.Errorf("import: no answer: %w", err)
			}
			nums, perr := parseNumbers(line, n)
			if perr == nil {
				return nums, nil
			}
			_, _ = fmt.Fprintln(w, perr)
		}
	}
	nums, err := ask("Images", len(p.Images))
	if err != nil {
		return sel, err
	}
	for _, i := range nums {
		sel.Images = append(sel.Images, p.Images[i].ID)
	}
	nums, err = ask("Volumes", len(p.Volumes))
	if err != nil {
		return sel, err
	}
	for _, i := range nums {
		sel.Volumes = append(sel.Volumes, p.Volumes[i].Name)
	}
	return sel, nil
}

// parseNumbers parses "1,3-4", "all" or "" against a list of n items into 0-based
// indexes, in order and without repeats.
func parseNumbers(s string, n int) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.EqualFold(s, "all") {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	seen := map[int]bool{}
	var out []int
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || a < 1 || b > n || a > b {
			return nil, fmt.Errorf("%q: give numbers from 1 to %d", part, n)
		}
		for i := a; i <= b; i++ {
			if !seen[i] {
				seen[i] = true
				out = append(out, i-1)
			}
		}
	}
	return out, nil
}

// writeResult prints what the import did and the check of Docker Desktop.
func writeResult(w io.Writer, res *desktopimport.Result, notes []string) {
	counts := map[desktopimport.Outcome]int{}
	already := map[string]int{}
	for _, it := range res.Items {
		counts[it.Outcome]++
		if it.Outcome == desktopimport.OutcomeAlready {
			already[it.Kind]++
		}
	}
	tw := newTable(w, "KIND", "NAME", "OUTCOME", "COPIED", "NOTE")
	for _, it := range res.Items {
		bytes := ""
		if it.Bytes > 0 {
			bytes = humanSize(uint64(it.Bytes))
		}
		tw.row(it.Kind, string(printable([]byte(it.Name))), string(it.Outcome), bytes, string(printable([]byte(redact.String(it.Reason)))))
	}
	for _, it := range res.Skipped {
		tw.row(it.Kind, string(printable([]byte(it.Name))), "skipped", "", string(printable([]byte(it.Reason))))
	}
	tw.flush()
	if len(res.Items) > 0 && counts[desktopimport.OutcomeAlready] == len(res.Items) {
		_, _ = fmt.Fprintf(w, "Nothing to import: %s and %s already here.\n", countText(already[desktopimport.KindImage], "image"), countText(already[desktopimport.KindVolume], "volume"))
	} else {
		_, _ = fmt.Fprintf(w, "Imported %d, tagged %d, already here %d, refused %d, failed %d, canceled %d; %s copied.\n",
			counts[desktopimport.OutcomeImported], counts[desktopimport.OutcomeTagged], counts[desktopimport.OutcomeAlready],
			counts[desktopimport.OutcomeRefused], counts[desktopimport.OutcomeFailed], counts[desktopimport.OutcomeCanceled],
			humanSize(uint64(max(res.Bytes, 0))))
	}
	sc := res.Source
	if sc.Unchanged {
		_, _ = fmt.Fprintf(w, "Docker Desktop unchanged: %s, %s, %s (%d volume contents compared by hash).\n",
			countText(sc.Images, "image"), countText(sc.Volumes, "volume"), countText(sc.Containers, "container"), sc.HashedVolumes)
	} else {
		_, _ = fmt.Fprintln(w, "Docker Desktop CHANGED during the import (hyperbutler only read it):")
		for _, d := range sc.Differences {
			_, _ = fmt.Fprintln(w, "  -", string(printable([]byte(d))))
		}
	}
	for _, s := range sc.Warnings {
		_, _ = fmt.Fprintln(w, "Warning:", string(printable([]byte(s))))
	}
	for _, n := range notes {
		_, _ = fmt.Fprintln(w, "Note:", string(printable([]byte(n))))
	}
	_, _ = fmt.Fprintln(w, "Containers are not moved: create them again in hyperbutler (docker compose up, or your docker run lines).")
}

func countText(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// importProgress prints byte counts and rates on stderr, at most twice a second, on one
// line that it rewrites (a console) or every 10 seconds (a file or pipe).
type importProgress struct {
	w       io.Writer
	quiet   bool
	console bool
	last    time.Time
	start   time.Time
	name    string
	shown   bool
}

func newImportProgress(w io.Writer, quiet bool) *importProgress {
	f, ok := w.(*os.File)
	return &importProgress{w: w, quiet: quiet, console: ok && isConsole(f)}
}

var phaseText = map[string]string{
	desktopimport.PhaseCopy:     "copying",
	desktopimport.PhaseVerify:   "verifying",
	desktopimport.PhaseSnapshot: "hashing Docker Desktop's",
	desktopimport.PhaseCompare:  "comparing",
}

func (p *importProgress) event(e desktopimport.Event) {
	switch e.Kind {
	case desktopimport.EventNote:
		p.clear()
		_, _ = fmt.Fprintln(p.w, "hyperbutler: import:", string(printable([]byte(e.Message))))
		return
	case desktopimport.EventStart:
		p.clear()
		if !p.quiet {
			_, _ = fmt.Fprintf(p.w, "%s %s...\n", e.Item, string(printable([]byte(e.Name))))
		}
		return
	case desktopimport.EventDone:
		p.clear()
		return
	}
	if p.quiet || e.Kind != desktopimport.EventProgress {
		return
	}
	key := e.Phase + e.Name
	now := time.Now()
	if key != p.name {
		p.name, p.start, p.last = key, now, time.Time{}
	}
	every := 500 * time.Millisecond
	if !p.console {
		every = 10 * time.Second
	}
	if now.Sub(p.last) < every || e.Bytes < 1<<20 {
		return
	}
	p.last = now
	rate := ""
	if s := now.Sub(p.start).Seconds(); s > 0.2 {
		rate = ", " + humanSize(uint64(float64(e.Bytes)/s)) + "/s"
	}
	of := ""
	if e.Total > 0 {
		of = " of " + humanSize(uint64(e.Total))
	}
	line := fmt.Sprintf("  %s %s %s: %s%s%s", phaseText[e.Phase], e.Item, string(printable([]byte(e.Name))), humanSize(uint64(e.Bytes)), of, rate)
	if p.console {
		_, _ = fmt.Fprintf(p.w, "\r%-78s", line)
		p.shown = true
	} else {
		_, _ = fmt.Fprintln(p.w, line)
	}
}

func (p *importProgress) clear() {
	if p.shown {
		_, _ = fmt.Fprintf(p.w, "\r%-78s\r", "")
		p.shown = false
	}
}
