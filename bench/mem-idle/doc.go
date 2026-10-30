// Command mem-idle measures REQ-MEM-001 (docs/requirements.md): host memory of an idle
// hyperbutler, sampled every 10 s from minute 5 to minute 10 after `hyperbutler start`,
// with the VM running and no containers.
//
// Host memory is the private working set of the engine (hyperbutler.exe), this VM's
// vmmem and vmwp, and any child process of the engine. The engine runs with its
// internal/diag endpoint on, and every sample also records the engine's
// runtime.MemStats, so GC slack can be told apart from live heap.
//
//	mem-idle -hyperbutler hyperbutler.exe -home %TEMP%\hb-bench -image-dir DIR -docker docker.exe
//
// It needs Hyper-V, so it does not run in GitHub-hosted CI; see README.md.
package main
