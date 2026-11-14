// Command soak is the leak gate of ADR 0003 (guardrail 3). It starts a bench
// installation with `hyperbutler start` (or attaches to a running engine), drives it with
// the stock docker CLI in a mixed loop (version, load, run, run+logs+rm, a bind mount of a
// Windows directory, a published port), and samples the engine's private working set,
// handle and thread counts, its goroutines and Go heap through the internal/diag
// endpoint, and the VM's vmmem and vmwp private working sets. After the run it fits a
// Theil–Sen slope to each metric over the steady-state window (after -warmup) and fails
// if any metric's fitted growth across that window is above its noise limit, or if there
// is too little data to tell. The engine's memory is judged on its private bytes with
// the Go heap the scavenger has returned added back (private_bytes_adj), since on Windows
// reusing those pages commits them again, held to the growth the private working set's
// limit allows; the working set, raw and adjusted, is reported with the working-set
// guard's trims but not judged, since a trim or an outside read moves it without a leak.
// The raw private bytes are judged only when heap_released is not sampled.
//
//	soak -hyperbutler hyperbutler.exe -home DIR -image-dir DIR -docker docker.exe \
//	     -image-tar busybox-image.tar -share-dir DIR -duration 24h -warmup 1h -out soak.csv
//	soak -judge soak.csv -warmup 1h    # re-judge a recorded run
//
// It needs a hypervisor, so it does not run in GitHub-hosted CI; see README.md.
package main
