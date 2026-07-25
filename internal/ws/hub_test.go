package ws

import "testing"

// Per-trip event sequence numbers must be monotonic and independent per trip.
//
// The client uses a gap in seq to decide it missed an event and must refetch
// GET /trip/:id, so a repeated counter would mask a real gap and a shared one
// would cause a spurious refetch on every message.
func TestBroadcastToTrip_SeqIsMonotonicPerTrip(t *testing.T) {
	h := NewHub()
	go h.Run()

	emit := func(tripID string, n int) []int64 {
		t.Helper()
		var out []int64
		for i := 0; i < n; i++ {
			h.BroadcastToTrip(tripID, Event{Type: "driver_location"})
			out = append(out, h.lastSeq(tripID))
		}
		return out
	}

	for i, got := range emit("trip-a", 4) {
		if want := int64(i + 1); got != want {
			t.Errorf("trip-a event %d has seq %d, want %d", i, got, want)
		}
	}

	if first := emit("trip-b", 2)[0]; first != 1 {
		t.Errorf("a second trip must start its own sequence, got %d", first)
	}
	if got := h.lastSeq("trip-a"); got != 4 {
		t.Errorf("trip-a sequence disturbed by another trip: %d", got)
	}

	// Finishing a trip releases its counter so the map cannot grow forever.
	h.ForgetTrip("trip-a")
	if got := h.lastSeq("trip-a"); got != 0 {
		t.Errorf("after ForgetTrip the counter should be released, got %d", got)
	}
}

// An empty trip id must not allocate a counter.
func TestBroadcastToTrip_IgnoresEmptyTripID(t *testing.T) {
	h := NewHub()
	go h.Run()
	h.BroadcastToTrip("", Event{Type: "noise"})
	if got := h.lastSeq(""); got != 0 {
		t.Errorf("empty trip id should be ignored, got seq %d", got)
	}
}
