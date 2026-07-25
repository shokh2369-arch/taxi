package ws

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Every waiting driver must learn about a dispatch change, not just one.
//
// The wake channel was buffered-1 and signalled with a send, so a single receive
// consumed the token: with N drivers long-polling, one woke and the rest waited
// out their timer. That is why the long poll had to re-query every second and
// why a "burst" of staggered pokes existed to paper over it.
func TestWaitForDispatchChange_WakesAllWaiters(t *testing.T) {
	h := NewDispatchHub()

	const waiters = 25
	var wg sync.WaitGroup
	woke := make(chan struct{}, waiters)

	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			h.WaitForDispatchChange(context.Background(), 5*time.Second)
			// Woken by the signal, not by the timeout.
			if time.Since(start) < 4*time.Second {
				woke <- struct{}{}
			}
		}()
	}

	// Give every goroutine time to enter the wait.
	time.Sleep(200 * time.Millisecond)
	h.signalPollWaiters()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("waiters did not return")
	}

	if got := len(woke); got != waiters {
		t.Errorf("%d of %d waiters woke on a single signal; the rest waited out the timeout", got, waiters)
	}
}

// A signal with nobody waiting must not leave a token that instantly wakes the
// next caller with stale news.
func TestWaitForDispatchChange_NoStaleWake(t *testing.T) {
	h := NewDispatchHub()
	h.signalPollWaiters() // nobody is waiting

	start := time.Now()
	h.WaitForDispatchChange(context.Background(), 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("waiter returned after %s; a signal sent while nobody waited must not be retained", elapsed)
	}
}

func TestWaitForDispatchChange_RespectsContext(t *testing.T) {
	h := NewDispatchHub()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	start := time.Now()
	h.WaitForDispatchChange(ctx, 5*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancelling the request context should end the wait, took %s", elapsed)
	}
}
