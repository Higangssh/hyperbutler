// Command hyperbutler-dev is a DEVELOPMENT-ONLY harness for the hvsock milestone. It is not
// the hyperbutler CLI, has no stable interface, and may be deleted at any time.
//
//	hyperbutler-dev run [flags]       boot the guest through HCS and serve the Docker API on
//	                             \\.\pipe\docker_engine_hyperbutler until Ctrl+C
//	hyperbutler-dev sandbox [flags]   boot the agent sandbox image, copy a project in through
//	                             sbxd and list the session's changes (ADR 0009)
//	hyperbutler-dev mkimage [flags]   write a `docker load` tar of a scratch image holding
//	                             one static busybox, without any network access
//
// See docs/research/hvsock-path.md for how it is used and what it measured. The VM
// lifecycle itself lives in internal/engine; `run` only turns flags into engine.Config.
// The user-facing CLI is cmd/hyperbutler.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "sandbox":
		err = sandboxCmd(os.Args[2:])
	case "mkimage":
		err = mkimageCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hyperbutler-dev:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: hyperbutler-dev run|sandbox|mkimage [flags]   (development harness, see -h of each)")
	os.Exit(2)
}
