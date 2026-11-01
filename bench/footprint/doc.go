// Command footprint measures the GUI's budgets (REQ-UX-011, ADR 0008 "Toolkit spike
// results"): it launches `hyperbutler gui --wait --report-paint`, waits for the first
// paint, samples every process the window has (hyperbutler.exe and its WebView2
// msedgewebview2.exe descendants) for private working set and count, checks that they
// hold no network socket, closes the window and checks that no process is left 2 s
// later. It repeats that -runs times (50 by default; 20 is enough for a local run) and
// fails unless every run is within the budgets:
//
//   - at most 100 MiB private working set, summed over the window's processes;
//   - at most 8 processes;
//   - first paint within 2 s, warm (the median over the runs after the first);
//   - no process left 2 s after the window closes, in any run.
//
// The window is measured with the engine stopped (the launch path needs no engine),
// so the numbers are the window's own. The state directory is HYPERBUTLER_HOME, as for
// the CLI; use a test one.
//
//	footprint -hb %TEMP%\hb-dev\gui\bin\hyperbutler.exe -runs 50
//	footprint -hb ... -runs 1 -lang pseudo -check-layout
//	footprint -shot-pid PID -out shot.png     (a PrintWindow screenshot of a window)
package main
