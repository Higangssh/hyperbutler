# bench/mem-reclaim: REQ-MEM-002

Measures [REQ-MEM-002](../../docs/requirements.md) as specified: the VM is configured with
a 12 GB maximum (`--memory 12GiB`), a container fills 4 GiB of anonymous memory plus 4 GiB
of page cache and exits, and within 60 s, without restarting the VM, host memory must
return to at most the REQ-MEM-001 target + 100 MB (500 MB for 0.1, 350 MB for 1.0 with
`-target-mb 250`). MB is 10^6 bytes.

- **Host memory** is what `bench/mem-idle` counts: the private working set of the engine,
  this VM's `vmmem` and `vmwp`, and the engine's children. `vmmem`'s private bytes (its
  commit) are recorded too, for diagnosis. A non-elevated user cannot open `vmmem` or
  `vmwp`, so the VM is identified the way `bench/mem-idle` does it (the one new `vmmem`
  under a new `vmwp` across `hyperbutler start`), and the counters come from
  `Win32_PerfFormattedData_PerfProc_Process`. What the guest does with its memory is
  read from inside: `/proc/meminfo` at the peak and after the window, and the engine
  log's `guest memory` and `guest page-cache reclaim` lines.
- **Workload.** `bench/mem-guest` is built for linux/amd64 and imported as the only file
  of a scratch image (`docker import`), so no registry is needed. The bench builds it
  from its own source directory; a bench binary built with `-trimpath` needs `-guest`. `mem-guest fill` maps
  and touches `-anon` bytes, then writes a `-cache` byte file to a named volume, syncs and
  reads it back, prints `/proc/meminfo` and exits. The file stays on the volume, so its
  page cache outlives the container (a file in the container's own layer would be
  deleted with `--rm`, and its cache with it, which would make the test trivial).
- **Timeline.** Start; wait `-settle` (3 min); sample the idle baseline for `-baseline`
  (1 min); run the fill container, sampling throughout; then sample for `-window` (3 min)
  after it exits. Every `-interval` (2 s).
- **Verdict.** It passes if, from some sample at or before +60 s until the end of the
  window, every sample is at or below the limit. A dip under the limit that does not
  last does not count, and neither does a total that was under the limit at +60 s but
  rose above it later in the window: this reads "returns to" as "and stays". The engine
  and VM (`vmmem` + `vmwp`) split at baseline and at the end is printed next to the
  verdict.
- **Not covered:** the file is synced before the container exits, so the page cache is
  clean; a container that exits with dirty pages also waits for writeback. MiB and GiB
  are used for the workload (4 GiB, slightly more than the requirement's 4 GB). The run is *not valid* if another VM or engine ran during it, if
  host memory did not grow by at least half the workload, or if the samples end before
  +60 s.
- It refuses to run if the installation is already running, uses its own Docker pipe
  with `-pipe`, removes the volume at the end (`-keep-volume` keeps it) and stops the
  engine unless `-keep-running`.

```powershell
mem-reclaim.exe -hyperbutler hyperbutler.exe -home $env:TEMP\hb-bench\home -image-dir DIR `
    -docker C:\docker\docker.exe -pipe \\.\pipe\docker_engine_hb_bench -out bench\results\mem-reclaim.csv
```

It needs Hyper-V and about 9 GB of free host memory, so CI runs only its unit tests.
Results are in [docs/research/mem-reclaim.md](../../docs/research/mem-reclaim.md).
