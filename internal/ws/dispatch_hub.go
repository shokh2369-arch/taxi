package ws

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"taxi-mvp/internal/safe"
)

// DispatchHub is a best-effort pub/sub hub for driver dispatch pokes.
//
// It intentionally does NOT carry any rider/trip payload; it is only a "refetch"
// signal for polling GET /driver/available-requests.
//
// All broadcasting is non-blocking: if clients are slow, messages may be dropped.
type DispatchHub struct {
	register   chan *dispatchClient
	unregister chan *dispatchClient
	broadcast  chan []byte

	clients map[*dispatchClient]struct{}

	version      atomic.Int64
	updatedAtUTC atomic.Int64 // unix nano
	// wake is closed (not sent to) to broadcast "dispatch changed" to every
	// long-poll waiter, then replaced. Guarded by wakeMu.
	wakeMu sync.RWMutex
	wake   chan struct{} // signals GET /driver/available-requests long-poll waiters
}

// DispatchHubDefault is an optional global hub used by publisher hooks.
// It may be nil; NotifyDispatchChanged is a no-op in that case.
var DispatchHubDefault *DispatchHub

func NewDispatchHub() *DispatchHub {
	return &DispatchHub{
		register:   make(chan *dispatchClient, 128),
		unregister: make(chan *dispatchClient, 128),
		broadcast:  make(chan []byte, 256),
		clients:    make(map[*dispatchClient]struct{}),
		wake:       make(chan struct{}),
	}
}

func (h *DispatchHub) Run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = struct{}{}
		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}
		case msg := <-h.broadcast:
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					// Best-effort: drop for slow clients.
				}
			}
		}
	}
}

// NotifyDispatchChanged broadcasts a "dispatch_changed" poke.
// It is safe to call from any goroutine and must never block request handlers.
func (h *DispatchHub) NotifyDispatchChanged() {
	if h == nil {
		return
	}
	now := time.Now().UTC()
	h.version.Add(1)
	h.updatedAtUTC.Store(now.UnixNano())

	payload := map[string]string{
		"type":       "dispatch_changed",
		"emitted_at": now.Format(time.RFC3339),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	select {
	case h.broadcast <- b:
	default:
		// Best-effort: drop if hub is under backpressure.
	}
	h.signalPollWaiters()
}

// WaitForDispatchChange blocks until a dispatch poke, ctx cancel, or maxWait elapses.
// Used by GET /driver/available-requests?wait_sec= so offers return without 500ms polling gaps.
func (h *DispatchHub) WaitForDispatchChange(ctx context.Context, maxWait time.Duration) {
	if h == nil || maxWait <= 0 {
		return
	}
	// Snapshot the current generation channel, then wait for it to be closed.
	// A close wakes every waiter; the previous buffered-1 send woke exactly one,
	// so with N drivers long-polling only one learned about a new offer and the
	// rest sat out their timer — which is why the poll loop had to re-query every
	// second, and why the burst helper existed to fire several pokes in a row.
	h.wakeMu.RLock()
	wake := h.wake
	h.wakeMu.RUnlock()

	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-wake:
	case <-timer.C:
	}
}

// signalPollWaiters wakes every current waiter by closing the generation channel
// and installing a fresh one for the next round.
func (h *DispatchHub) signalPollWaiters() {
	if h == nil {
		return
	}
	h.wakeMu.Lock()
	close(h.wake)
	h.wake = make(chan struct{})
	h.wakeMu.Unlock()
}

// NotifyDispatchChanged is the global, best-effort publisher hook.
func NotifyDispatchChanged() {
	if DispatchHubDefault == nil {
		return
	}
	DispatchHubDefault.NotifyDispatchChanged()
}

// NotifyDispatchChangedBurst sends several staggered pokes so drivers refetch soon even when a single
// non-blocking broadcast or full client buffer drops the first message (common under load).
func NotifyDispatchChangedBurst() {
	NotifyDispatchChanged()
	safe.Go("dispatch_changed_burst", func() {
		time.Sleep(120 * time.Millisecond)
		NotifyDispatchChanged()
		time.Sleep(280 * time.Millisecond)
		NotifyDispatchChanged()
	})
}
