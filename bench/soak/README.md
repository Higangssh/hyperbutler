# bench/soak: leak gate

`soak` is guardrail 3 of [ADR 0003](../../docs/adr/0003-implementation-language.md). It
drives a hyperbutler engine with the stock docker CLI and fails if the engine process,
or the VM, grows over time.

## What it does

- **Engine.** In launch mode (`-hyperbutler`, `-home`) the soak runs `hyperbutler start
  --no-context --debug-pipe ...` itself for a bench installation, finds the engine PID in
  its `state.json` and the VM's `vmmem` and `vmwp` as the pair that appeared during the
  start, and runs `hyperbutler stop` at the end. Attach mode (`-pid`, `-vmmem-pid`,
  `-vmwp-pid`, `-diag-pipe`) samples an engine that is already running.
- **Workload**, in a loop: `docker version`, `docker load -i`, `docker run --rm`,
  `docker run --name`, `docker logs` (must match the run's output), `docker rm -f`;
  with `-share-dir`, a bind mount of a Windows directory (`docker run -v DIR:/mnt`: the
  container reads a file the host wrote, and writes one the host reads back); with
  `-port` as well, a published port (`docker run -d -p P:8080 -v DIR\www:/www:ro ...`,
  a one-page HTTP server made of busybox `nc`, then an HTTP GET from the host that must return the page the host
  wrote, then `docker rm -f`). Every operation must succeed (`-max-errors 0` by default).
- **Samples**, every `-interval`, read in process from
  `NtQuerySystemInformation(SystemProcessInformation)` (the source of
  `Win32_PerfFormattedData_PerfProc_Process`; before 2026-10-31 a PowerShell CIM query per
  sample, which made an outside process scan the engine, see
  [research/mem-reclaim.md](../../docs/research/mem-reclaim.md)): the
  engine's private working set (the REQ-MEM-001 metric), private bytes, handle and thread
  counts, and the VM's `vmmem` and `vmwp` private working sets (the engine empties its
  working set when it rises to its commit, `internal/diag.WorkingSetGuard`, and logs
  `working set emptied`; such a trim shows as a drop in the private working set, so read
  a trend in it next to the private bytes, which a trim does not change); from the engine's
  `internal/diag` endpoint its goroutine count and Go heap in use. The CSV also records,
  unjudged, heap alloc, idle, released, stacks, Go `Sys`, the GC count, and any other VM
  or engine running on the host at the time (`foreign`).
- **Profiles.** With `-profile-dir`, heap, allocs and goroutine profiles and a stats
  document are fetched from the diag endpoint at the end of the warm-up and at the end of
  the run, while the workload still runs (`go tool pprof`).
- **Verdict**, per metric, over the samples after `-warmup`: each `-bucket` (default
  5 min) is reduced to its floor (10th percentile), which removes the GC sawtooth as long
  as the bucket is longer than its period (Go forces a GC at least every 2 minutes). Two
  tests run on the floors, and either fails the metric when it exceeds
  `max(abs, rel × median)` (flags `-ws-abs`, `-handles-abs`, ...):
  - the Theil–Sen slope times the window length (steady leaks);
  - the median floor of the last fifth of the window minus that of the first fifth
    (leaks that start late, and steps, which a robust slope ignores).

  The run also fails if the engine or the VM exits, if any docker command fails, if
  samples or buckets are missing, or if the window is too short to judge. A metric
  with no samples fails unless explicitly opted out (`-diag-pipe none`,
  `-vmmem-pid -1`, `-vmwp-pid -1` in attach mode).

The engine PID is held open for the whole run, so it cannot be reused by another process.
Other VMs on the host do not change this engine's counters but compete for memory, so
they are noted per sample rather than failing the run.

## Running it

```powershell
hyperbutler-dev.exe mkimage -busybox busybox.static -o busybox-image.tar
mkdir $env:TEMP\hb-soak\share
# main gate
soak.exe -hyperbutler hyperbutler.exe -home $env:TEMP\hb-soak\home -image-dir DIR `
    -docker C:\docker\docker.exe -image-tar busybox-image.tar -share-dir $env:TEMP\hb-soak\share `
    -duration 1h -warmup 10m -profile-dir $env:TEMP\hb-soak\prof -out bench\results\soak.csv
# release gate
soak.exe ... -duration 24h -warmup 1h
# quick local check (catches only fast leaks)
soak.exe ... -duration 15m -warmup 3m -bucket 1m
```

Raw CSVs and profiles go under `bench/results/`, which git ignores; record summaries in
`docs/`.

## Where it runs

The soak needs Hyper-V (HCS) and a booted guest. GitHub-hosted runners have no nested
virtualization, so **CI does not run it**. The 24 h run that gates a release, and the
shorter (about 1 h) run that gates main, run on self-hosted Windows hardware. CI runs the
unit and fuzz tests of the trend analysis and the sample parsers
(`bench/internal/hostmon`), and every package's tests run under `goleak` (guardrail 1).

Growth confined to the last ~5 % of the window (about the last hour of a 24 h run) is
not reliably caught; the shift test looks at the last fifth and the last tenth.

A short run only catches fast leaks. For a process at 40 MiB the default private
working set limit is max(1 MiB, 5 % × 40 MiB) = 2 MiB of growth, so a 10-minute window
fails a leak above about 12 MiB/h and a 50-minute window one above about 2.4 MiB/h.
