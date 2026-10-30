// Command winbox-dev is a DEVELOPMENT-ONLY harness for the hvsock milestone. It is not
// the winbox CLI, has no stable interface, and may be deleted at any time.
//
//	winbox-dev run [flags]       boot the guest through HCS and serve the Docker API on
//	                             \\.\pipe\docker_engine_winbox until Ctrl+C
//	winbox-dev mkimage [flags]   write a `docker load` tar of a scratch image holding
//	                             one static busybox, without any network access
//
// See docs/research/hvsock-path.md for how it is used and what it measured.
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
	case "mkimage":
		err = mkimageCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "winbox-dev:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: winbox-dev run|mkimage [flags]   (development harness, see -h of each)")
	os.Exit(2)
}
