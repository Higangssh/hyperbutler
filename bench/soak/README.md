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
  `internal/diag` endpoint its goroutine count and Go heap in use. When the HCS access broker
  service (`hyperbutler-broker.exe`, ADR 0004) runs, its private bytes and handle count
  are sampled and judged too (`-broker-abs`, `-broker-rel`; handles with the engine's
  limits), and the run fails if the service stops or restarts. The CSV also records,
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

  The engine's private working set and private bytes are **judged with the Go heap the
  scavenger returned added back**: `ws_private_adj = ws_private + heap_released` and
  `private_bytes_adj = private_bytes + heap_released`, from the same sample, with the
  same limits (`-ws-abs`, `-ws-rel`). On Windows the Go scavenger decommits the pages it
  returns to the OS, and reusing them commits them again, so the raw metrics move
  opposite to `heap_released`, by up to its size (several MiB under the soak). That is
  not a leak, but in a 1 h run it exceeds the 2 MiB allowed for a 40 MiB working set. The
  adjusted metrics cancel it and still rise with any growth the runtime cannot give back
  (Go heap, stacks, native memory). The raw `ws_private` and `private_bytes` stay in the
  CSV and the table, marked `reported, not judged (see ws_private_adj)` with what their
  verdict alone would be. Without `heap_released` (attach mode with `-diag-pipe none`)
  the adjusted metrics are skipped and the raw ones are judged instead, which the table
  says. Measured 2026-11-01 (1 h run, 50 min window): `ws_private` +3.4 MiB, while
  `private_bytes + heap_released` went from 93.77 to 93.80 MiB and
  `ws_private + heap_released` from 44.9 to 45.2 MiB; Go `Sys` and the Windows heaps
  were flat, and a diff of the committed regions put the growth inside Go's heap arenas.

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
# re-judge a recorded run from its CSV, with the run's -warmup and -bucket
soak.exe -judge bench\results\soak.csv -warmup 10m
```

`-judge` reads the columns by name, so CSVs from before a column existed still work; the
adjusted metrics are recomputed from `ws_private`, `private_bytes` and `heap_released`.
Metrics with no samples in the CSV are skipped (the CSV does not record opt-outs), the
workload's results are not in the CSV, and `-min-window` defaults to 80 % of the recorded
time after the warm-up.

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
