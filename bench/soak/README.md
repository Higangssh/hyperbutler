# bench/soak: leak gate

`soak` is guardrail 3 of [ADR 0003](../../docs/adr/0003-implementation-language.md). It
drives a running hyperbutler engine with the stock docker CLI and fails if the host process,
or the VM, grows over time.

## What it does

- **Workload**, in a loop: `docker version`, `docker load -i`, `docker run --rm`,
  `docker run --name`, `docker logs` (must match the run's output), `docker rm -f`.
  Every docker command must succeed (`-max-errors 0` by default).
- **Samples**, every `-interval`, from `Win32_PerfFormattedData_PerfProc_Process`: the
  target's private working set (the REQ-MEM-001 metric), private bytes, handle and thread
  counts, and the VM's `vmmem` private working set. If the target serves an
  `internal/diag` endpoint (`-diag-pipe`), also its goroutine count and Go heap in use.
- **Verdict**, per metric, over the samples after `-warmup`: each `-bucket` (default
  5 min) is reduced to its floor (10th percentile), which removes the GC sawtooth as long
  as the bucket is longer than its period (Go forces a GC at least every 2 minutes). Two
  tests run on the floors, and either fails the metric when it exceeds
  `max(abs, rel × median)` (flags `-ws-abs`, `-handles-abs`, ...):
  - the Theil–Sen slope times the window length (steady leaks);
  - the median floor of the last fifth of the window minus that of the first fifth
    (leaks that start late, and steps, which a robust slope ignores).

  The run also fails if the target or the VM exits, if any docker command fails, if
  samples or buckets are missing, or if the window is too short to judge. A metric
  with no samples fails unless explicitly opted out (`-diag-pipe none`,
  `-vmmem-pid -1`).

The target PID is held open for the whole run, so it cannot be reused by another process.

## Running it

```powershell
hyperbutler-dev.exe run -kernel ...\vmlinuz-virt -initrd ...\initramfs-hyperbutler.gz -debug-pipe \\.\pipe\hyperbutler-dev-debug
# release gate
soak.exe -docker C:\docker\docker.exe -image-tar busybox-image.tar -diag-pipe \\.\pipe\hyperbutler-dev-debug `
    -duration 24h -warmup 1h -out bench\results\soak.csv
# main gate
soak.exe ... -duration 1h -warmup 10m
# quick local check (catches only fast leaks)
soak.exe ... -duration 15m -warmup 3m -bucket 1m
```

`hyperbutler-dev -debug-pipe` (the `internal/diag` endpoint) is not wired in yet; until it is,
pass `-diag-pipe none`, which skips the goroutine and heap metrics and says so.

`hyperbutler-dev mkimage` writes the image tar. Raw CSVs go under `bench/results/`, which git
ignores; record summaries in `docs/`.

## Where it runs

The soak needs Hyper-V (HCS) and a booted guest. GitHub-hosted runners have no nested
virtualization, so **CI does not run it**. The 24 h run that gates a release, and the
shorter (about 1 h) run that gates main, run on self-hosted Windows hardware. CI runs the
unit and fuzz tests of the trend analysis and the sample parser, and every package's
tests run under `goleak` (guardrail 1).

Growth confined to the last ~5 % of the window (about the last hour of a 24 h run) is
not reliably caught; the shift test looks at the last fifth and the last tenth.

A short run only catches fast leaks. For a process at 40 MiB the default private
working set limit is max(1 MiB, 5 % × 40 MiB) = 2 MiB of growth, so a 10-minute window
fails a leak above about 12 MiB/h and a 23-hour window one above about 90 KiB/h.
