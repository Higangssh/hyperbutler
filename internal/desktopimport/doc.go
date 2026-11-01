// Package desktopimport copies images and volumes from Docker Desktop's engine into
// hyperbutler's (REQ-UX-023). It is a copy, never a move: Docker Desktop's images,
// volumes and containers are unchanged and keep working, and every run checks that.
// `hyperbutler import --from-desktop` is a thin layer over it; a GUI can use it the
// same way.
//
// # API
//
//	ep, err := desktopimport.Discover(ctx, opts)   // is Docker Desktop running, where
//	im, err := desktopimport.Open(ctx, opts)       // Discover, and hyperbutler answers
//	defer im.Close()
//	plan, err := im.Plan(ctx, desktopimport.PlanOptions{CompareVolumes: true})
//	res, err := im.Run(ctx, plan, desktopimport.Selection{AllImages: true}, func(e desktopimport.Event) { ... })
//	ok := res.OK()
//
// Options.Dial opens a named pipe (winio.DialPipeContext); Options.TargetPipe is
// hyperbutler's pipe (state.json's); Options.Home its state directory (default
// engine.DefaultHome). Discover and Open fail with errors wrapping
// ErrDesktopNotRunning ("start Docker Desktop first"), ErrTargetNotRunning or
// ErrSameEngine.
//
// Plan lists Docker Desktop's images and volumes with a Status each: StatusNew,
// StatusAlready (same image id and tags; a volume with the same content hash and
// labels), StatusPartial (the image is there, some tags are missing: Run only adds
// them), StatusConflict (hyperbutler has another image under a tag, or a volume of the
// same name with other content), StatusDifferentLabels (a volume with the same content
// and other labels), StatusExists (a volume whose content was not compared) or
// StatusUnsupported. VolumePlan.UsedBy names running Docker Desktop containers that
// mount a volume. Plan with CompareVolumes reads both volumes whole for each name
// present on both sides. Plan removes nothing: what a killed import left is listed in
// Plan.Recovery, and Run removes it. Plan.Select narrows a plan to a Selection with
// Run's name resolution (a dry run with names).
//
// Run imports a Selection and returns a Result: one ItemResult per selected item with
// its Outcome (imported, tagged, already, refused, failed, canceled), the bytes copied,
// and SourceCheck, the comparison of Docker Desktop before and after. The progress
// callback gets an Event for each item's start and end, byte counts while streaming
// (Phase copy, verify, snapshot, compare) and notes. Canceling ctx stops the import:
// the item in progress is removed from hyperbutler, the others are canceled, and the
// check of Docker Desktop still runs. Run takes the installation's import lock
// (ErrImportRunning) and checks the free space first (ErrNoSpace, unless
// Selection.ForceSpace; Importer.Space shows the numbers), so every caller, the CLI
// and a GUI, gets both. Run does not retry. What is "already imported" is always what
// hyperbutler has now; imports-complete.json in Home only records verified volumes so
// that recovery never removes them.
//
// # Docker Desktop is only read
//
// Every request to Docker Desktop goes through package source, whose Client sends GETs
// to a fixed list of read-only endpoints and nothing else, apart from one exception:
// copying a volume needs the archive API, which works on a container, so a helper
// container is created that mounts the volume read-only (no network, an entrypoint that
// does not exist, label dev.hyperbutler.import=helper and the run's id), from an image
// already there that declares no VOLUME and is for the engine's platform (so nothing is
// pulled and no anonymous volume is made), never started, and removed afterwards with
// v=1, also on an error or a cancel. A helper left by a killed import is removed by the
// next one, once the run that made it is gone (runAlive).
//
// Before the import Run records Docker Desktop's images and tags, volume names, the
// content hash of every volume it will read, and its containers with their states;
// afterwards it records them again and SourceCheck lists any difference (and any
// helper still there). Result.OK is false when anything differs.
//
// # Copies and verification
//
// An image is streamed from GET /images/get into POST /images/load, never buffered
// whole, and then every tag in hyperbutler must name the same image id (or, when one
// engine uses containerd's image store and the other does not, the same layers,
// creation time and platform). A volume is streamed from the Docker Desktop helper's
// archive through a checker that refuses entries outside the volume and hashes names,
// types, modes, owners, sizes, link targets and contents (times and extended
// attributes are not compared), into PUT /containers/{id}/archive of a helper in
// hyperbutler that mounts a new volume of the same name and labels; dockerd in the VM
// extracts it. The hash must equal the one recorded before (else the volume changed
// during the copy), and a fresh export of the copy must hash the same. Any failure
// removes the copy from hyperbutler, and only there.
package desktopimport
