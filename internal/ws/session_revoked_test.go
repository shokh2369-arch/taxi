package ws

import (
	"encoding/json"
	"testing"
	"time"
)

// A new login must poke the OLD device's dispatch socket with session_revoked —
// and only that driver's sockets.
func TestDispatchHub_NotifySessionRevoked_TargetsOnlyThatDriver(t *testing.T) {
	h := NewDispatchHub()
	go h.Run()

	old := &dispatchClient{hub: h, send: make(chan []byte, 8), userID: 7}
	other := &dispatchClient{hub: h, send: make(chan []byte, 8), userID: 8}
	h.register <- old
	h.register <- other
	// Run loop drains register asynchronously; give it a beat.
	time.Sleep(50 * time.Millisecond)

	h.NotifySessionRevoked(7)

	select {
	case msg := <-old.send:
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil || ev.Type != "session_revoked" {
			t.Fatalf("old device got %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old device never received session_revoked")
	}

	select {
	case msg := <-other.send:
		t.Fatalf("unrelated driver received %s", msg)
	case <-time.After(150 * time.Millisecond):
		// correct: nothing delivered
	}
}

func TestDispatchHub_NotifySessionRevoked_NilAndNoClients(t *testing.T) {
	var nilHub *DispatchHub
	nilHub.NotifySessionRevoked(1) // must not panic

	h := NewDispatchHub()
	go h.Run()
	h.NotifySessionRevoked(42) // no clients — must not block or panic
}

// The trip hub variant: only the revoked user's connections get the event.
func TestTripHub_NotifySessionRevoked(t *testing.T) {
	h := NewHub()
	go h.Run()

	old := &client{hub: h, send: make(chan []byte, 8), tripIDs: map[string]struct{}{"t1": {}}, userID: 7}
	rider := &client{hub: h, send: make(chan []byte, 8), tripIDs: map[string]struct{}{"t1": {}}, userID: 9}
	h.register <- old
	h.register <- rider
	time.Sleep(50 * time.Millisecond)

	h.NotifySessionRevoked(7)

	select {
	case msg := <-old.send:
		var ev Event
		if err := json.Unmarshal(msg, &ev); err != nil || ev.Type != "session_revoked" {
			t.Fatalf("driver got %s", msg)
		}
		if ev.EmittedAt == "" {
			t.Fatalf("session_revoked missing emitted_at: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("driver never received session_revoked")
	}

	select {
	case msg := <-rider.send:
		t.Fatalf("rider on same trip received %s", msg)
	case <-time.After(150 * time.Millisecond):
	}
}
