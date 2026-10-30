# bench/start: REQ-START-001

Measures [REQ-START-001](../../docs/requirements.md): from invoking `hyperbutler start`
with no VM running until `docker info` succeeds. Defaults: one warm-up run discarded,
then 20 valid runs; median and nearest-rank p95 against 8 s / 12 s (0.1; pass `-median 5s
-p95 8s` for 1.0).

- Each run waits for a quiet host (no other `vmmem`, `vmwp` or hyperbutler engine, the
  Docker API pipe free; `-wait-quiet`), watches the host every 250 ms while it runs, and
  is excluded and repeated if another VM or engine appeared. The bench's own CLI and
  docker processes are recognised as its descendants; its engine and VM by `state.json`
  and the snapshot taken before the start.
- After each run it runs `hyperbutler stop` (timed too) and waits until the VM's `vmmem`
  and `vmwp` are gone.
- The table also shows the engine's own `readyMillis` from `state.json` (VM creation until
  dockerd answered).
- **First start after a reboot** is not part of the 20: run `start.exe -n 1 -warmup 0`
  right after booting. The tool prints the host uptime and keeps a marker in the
  installation directory, so it can say whether a run was the first it saw since boot.

```powershell
start.exe -hyperbutler hyperbutler.exe -home $env:TEMP\hb-bench\home -image-dir DIR -docker C:\docker\docker.exe
```

It needs Hyper-V, so CI runs only its unit tests.
