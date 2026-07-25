// Package safe runs goroutines so that a panic in one of them cannot take down
// the process.
//
// This service runs as a single instance — Telegram allows one getUpdates
// connection per bot, so render.yaml pins numInstances to 1. Without recovery, a
// nil map or an out-of-range index anywhere in a background worker or a
// WebSocket pump terminates the whole process: every in-flight trip, every open
// socket, and dispatch for all riders and drivers at once.
package safe

import (
	"context"
	"log"
	"runtime/debug"
	"time"
)

// Restart backoff bounds for supervised workers.
const (
	minRestartDelay = 1 * time.Second
	maxRestartDelay = 30 * time.Second
	// A run lasting at least this long counts as healthy and resets the backoff.
	healthyRunDuration = 1 * time.Minute
)

// Recover logs a panic and lets the caller keep running.
// Use as the first statement of a goroutine: defer safe.Recover("name").
func Recover(name string) {
	if r := recover(); r != nil {
		log.Printf("panic recovered in %s: %v\n%s", name, r, debug.Stack())
	}
}

// Run calls fn on the current goroutine with panic recovery.
// It reports whether fn panicked.
func Run(name string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			log.Printf("panic recovered in %s: %v\n%s", name, r, debug.Stack())
		}
	}()
	fn()
	return false
}

// Go runs fn in a new goroutine with panic recovery. Use for one-shot work
// (per-connection pumps, per-request fan-out) where stopping on panic is fine.
func Go(name string, fn func()) {
	go func() {
		defer Recover(name)
		fn()
	}()
}

// GoSupervised runs fn in a goroutine and restarts it if it panics, backing off
// exponentially between 1s and 30s, until ctx is cancelled.
//
// Plain recovery is not enough for a long-running worker: the process would
// survive but the worker would be silently dead, so ride requests would stop
// expiring or offers would stop being dispatched with nothing to indicate why.
// A normal (non-panicking) return is treated as the worker finishing its job and
// is not restarted.
func GoSupervised(ctx context.Context, name string, fn func()) {
	go RunSupervised(ctx, name, fn)
}

// RunSupervised is GoSupervised on the current goroutine. Use it when the caller
// already owns a goroutine — for example one tracked by a sync.WaitGroup, where
// the deferred Done must still run when the supervisor gives up.
func RunSupervised(ctx context.Context, name string, fn func()) {
	delay := minRestartDelay
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		panicked := Run(name, fn)
		// A run that lasted a while was healthy, so the next failure should back
		// off from the start again. Without this, delay only ever grows: a worker
		// that panics once a month reaches the 30s cap and stays there, turning an
		// unrelated future panic into a 30s outage instead of a 1s one.
		if time.Since(started) >= healthyRunDuration {
			delay = minRestartDelay
		}
		if !panicked {
			return // returned normally: worker is done
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("safe: restarting %s in %s after panic", name, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > maxRestartDelay {
			delay = maxRestartDelay
		}
	}
}
