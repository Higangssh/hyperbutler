// Command soak is the leak gate of ADR 0003 (guardrail 3). It starts a bench
// installation with `hyperbutler start` (or attaches to a running engine), drives it with
// the stock docker CLI in a mixed loop (version, load, run, run+logs+rm, a bind mount of a
// Windows directory, a published port), and samples the engine's private working set,
// handle and thread counts, its goroutines and Go heap through the internal/diag
// endpoint, and the VM's vmmem and vmwp private working sets. After the run it fits a
// Theil–Sen slope to each metric over the steady-state window (after -warmup) and fails
// if any metric's fitted growth across that window is above its noise limit, or if there
// is too little data to tell.
//
//	soak -hyperbutler hyperbutler.exe -home DIR -image-dir DIR -docker docker.exe \
//	     -image-tar busybox-image.tar -share-dir DIR -duration 24h -warmup 1h -out soak.csv
//
// It needs a hypervisor, so it does not run in GitHub-hosted CI; see README.md.
package main
