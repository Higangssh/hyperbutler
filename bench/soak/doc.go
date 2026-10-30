// Command soak is the leak gate of ADR 0003 (guardrail 3). It drives a running winbox
// engine with the stock docker CLI in a mixed loop (version, load, run, run+logs+rm),
// samples the target process's private working set, handle and thread counts, the VM's
// vmmem private working set, and, when the target serves an internal/diag endpoint, its
// goroutine count and Go heap. After the run it fits a Theil–Sen slope to each metric
// over the steady-state window (after -warmup) and fails if any metric's fitted growth
// across that window is above its noise limit, or if there is too little data to tell.
//
//	soak -docker C:\docker\docker.exe -image-tar busybox-image.tar \
//	     -duration 24h -warmup 1h -out bench\results\soak.csv
//
// It needs a hypervisor, so it does not run in GitHub-hosted CI; see README.md.
package main
