package safe

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunReportsWhetherFnPanicked(t *testing.T) {
	if Run("boom", func() { panic("kaboom") }) != true {
		t.Error("Run should report that fn panicked")
	}
	if Run("fine", func() {}) != false {
		t.Error("Run should report no panic for a clean fn")
	}
}

func TestGoRecoversWithoutKillingTheProcess(t *testing.T) {
	done := make(chan struct{})
	Go("panicky", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine never ran")
	}
	// Reaching here at all means the panic did not propagate.
}

func TestGoSupervisedRestartsAfterPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	reached := make(chan struct{})
	var once sync.Once

	GoSupervised(ctx, "flaky", func() {
		if runs.Add(1) < 3 {
			panic("not yet")
		}
		once.Do(func() { close(reached) })
	})

	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatalf("worker was not restarted; runs=%d", runs.Load())
	}
	if got := runs.Load(); got < 3 {
		t.Errorf("runs = %d, want at least 3", got)
	}
}

// A worker that returns normally has finished its job and must not be respawned.
func TestGoSupervisedDoesNotRestartCleanReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runs atomic.Int32
	GoSupervised(ctx, "oneshot", func() { runs.Add(1) })

	time.Sleep(200 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Errorf("runs = %d, want exactly 1", got)
	}
}

func TestGoSupervisedStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var runs atomic.Int32
	GoSupervised(ctx, "cancelled", func() {
		runs.Add(1)
		panic("always")
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
	settled := runs.Load()
	time.Sleep(2 * time.Second)
	if got := runs.Load(); got > settled+1 {
		t.Errorf("worker kept restarting after cancel: %d -> %d", settled, got)
	}
}
