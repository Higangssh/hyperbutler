// Command mem-reclaim measures REQ-MEM-002 (docs/requirements.md): with the VM
// configured with a 12 GB maximum, a container fills 4 GB of anonymous memory plus
// 4 GB of page cache and exits; within 60 s, without restarting the VM, host memory
// must return to at most the REQ-MEM-001 target + 100 MB.
//
// Host memory is counted as bench/mem-idle counts it: the private working set of the
// engine (hyperbutler.exe), this VM's vmmem and vmwp, and the engine's child
// processes. The container is bench/mem-guest in a scratch image made with `docker
// import`, so the bench needs no registry.
//
//	mem-reclaim -hyperbutler hyperbutler.exe -home %TEMP%\hb-bench -image-dir DIR -docker docker.exe
//
// It needs Hyper-V, so it does not run in GitHub-hosted CI; see README.md.
package main
