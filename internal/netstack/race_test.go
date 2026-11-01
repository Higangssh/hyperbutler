//go:build race

package netstack

// raceEnabled reports whether the race detector is on. It slows the stacks down several
// times, so the long streaming tests send less under it.
const raceEnabled = true
