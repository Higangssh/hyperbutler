// Command start measures REQ-START-001 (docs/requirements.md): the time from invoking
// `hyperbutler start` with no VM running until `docker info` succeeds, over N runs
// (default 20) after one discarded warm-up run, as median and nearest-rank p95.
//
// Each run waits for a quiet host first (no other VM or hyperbutler engine, the Docker
// API pipe free), watches the host while it runs, and is excluded and repeated if
// another VM or engine appeared. After each run it stops the engine and waits until the
// VM's vmmem and vmwp processes are gone.
//
// The first start after a host reboot has to be measured separately: run this tool
// with -n 1 -warmup 0 right after booting. It reports the host uptime and whether the
// run was the first one it saw since boot.
//
//	start -hyperbutler hyperbutler.exe -home %TEMP%\hb-bench -image-dir DIR -docker docker.exe
package main
