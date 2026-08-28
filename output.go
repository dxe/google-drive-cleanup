package main

import (
	"log"
	"sync"
	"time"
)

// verbose is set by the persistent --verbose/-v flag (see main.go). When false
// (the default) the tool prints only progress summaries, warnings, errors, and
// final tallies; when true it also prints a line per item it touches.
var verbose bool

// progressInterval is how often a long-running move phase emits a heartbeat.
const progressInterval = 5 * time.Second

// progressEvery is how many items a counted phase gets through between
// heartbeat lines (see progress.step).
const progressEvery = 5

// progress throttles heartbeat lines during a long move phase so it shows it is
// advancing without flooding the log. Construct one with newProgress before the
// loop and call either tick after each item, for a line at most once per
// progressInterval, or step, for a line every progressEvery items. Heartbeats
// are suppressed under --verbose, where the per-item detailf lines already show
// progress.
//
// Both are safe to call from multiple goroutines, so a concurrent move phase can
// share one progress across its worker pool. A run with several phases wants a
// fresh progress per phase, so each phase's counting and clock start at zero.
type progress struct {
	mu   sync.Mutex
	last time.Time
	n    int
}

func newProgress() *progress {
	return &progress{last: time.Now()}
}

func (p *progress) tick(format string, args ...any) {
	if verbose {
		return
	}
	p.mu.Lock()
	if time.Since(p.last) < progressInterval {
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	p.mu.Unlock()
	log.Printf(format, args...)
}

// step is tick's counting sibling: it prints on every progressEvery-th call
// rather than once per progressInterval. Use it for a phase whose items are
// slow and few enough that a clock-driven heartbeat would mostly repeat itself,
// and where the count of items done is the interesting number. Like tick it is
// safe to call from several goroutines and is suppressed under --verbose.
func (p *progress) step(format string, args ...any) {
	if verbose {
		return
	}
	p.mu.Lock()
	p.n++
	due := p.n%progressEvery == 0
	p.mu.Unlock()
	if due {
		log.Printf(format, args...)
	}
}

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
