# bench/mem-idle: REQ-MEM-001

Measures [REQ-MEM-001](../../docs/requirements.md) as specified: host memory with the VM
running and no containers, sampled every 10 s from minute 5 to minute 10 after
`hyperbutler start` (31 samples), median against 400 MB (0.1) or 250 MB (1.0, `-target-mb
250`). MB here is 10^6 bytes.

- **Host memory** is the private working set (`Win32_PerfFormattedData_PerfProc_Process`)
  of the engine (`hyperbutler.exe`), this VM's `vmmem` and `vmwp`, and any child process of
  the engine.
- **Which VM is ours.** A non-elevated user cannot open `vmmem` or `vmwp` to read their VM
  id. The tool takes a process snapshot before `hyperbutler start` and after it, and
  takes the one new `vmmem` whose parent is a new `vmwp`. Anything else (none, or two VMs
  started at once) is an error.
- **GC slack vs. leak.** The engine runs with `--debug-pipe`, and every sample records its
  `runtime.MemStats` from `internal/diag`: heap alloc, heap in use, free but retained heap
  (`HeapIdle − HeapReleased` plus span fragmentation), stacks, Go `Sys`, the GC count and
  goroutines. The summary prints them next to the working set, with Theil–Sen slopes of the
  engine working set and of heap alloc over the window (5 minutes shows only fast growth;
  `bench/soak` is the leak test).
- **Other VMs.** Another user or agent may run VMs on the same machine. Before each
  attempt the tool waits (`-wait-quiet`) for no other `vmmem`, `vmwp` or hyperbutler engine
  and a free Docker API pipe. A sample taken while one runs makes the attempt *not valid*;
  it is repeated up to `-retries` times.
- It refuses to run if the installation is already running, and checks `docker ps` is
  empty at the start and the end. It stops the engine afterwards unless `-keep-running`.

```powershell
mem-idle.exe -hyperbutler hyperbutler.exe -home $env:TEMP\hb-bench\home -image-dir DIR `
    -docker C:\docker\docker.exe -out bench\results\mem-idle.csv
```

It needs Hyper-V, so CI runs only its unit tests.
