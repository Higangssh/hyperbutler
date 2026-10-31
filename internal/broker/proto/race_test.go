//go:build race

package proto

// raceEnabled reports whether the race detector is on. Its instrumentation allocates, so
// allocation counts measured under it say nothing about the decoder.
const raceEnabled = true
