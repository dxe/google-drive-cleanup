package main

import "log"

// verbose is set by the persistent --verbose/-v flag (see main.go). When false
// (the default) the tool prints only progress summaries, warnings, errors, and
// final tallies; when true it also prints a line per item it touches.
var verbose bool

// detailf logs a per-item detail line, but only when --verbose is set. Use it
// for the "OK moved X -> Y", "listing folder Z", "SKIP ..." chatter that is
// useful when debugging but drowns the summary on a large Drive. Errors,
// warnings, and end-of-run tallies should use log.Printf directly so they
// always show.
func detailf(format string, args ...any) {
	if verbose {
		log.Printf(format, args...)
	}
}
