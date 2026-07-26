package ws

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxMessageSize = 512

var (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

// Event is sent to clients. Each event includes type, trip_id, trip_status, and emitted_at for reliability.
type Event struct {
	Type       string      `json:"type"`
	TripID     string      `json:"trip_id,omitempty"`
	TripStatus string      `json:"trip_status,omitempty"`
	EmittedAt  string      `json:"emitted_at,omitempty"` // RFC3339
	Payload    interface{} `json:"payload,omitempty"`
	// Seq is a per-trip monotonic sequence number.
	//
	// Delivery is best effort in both directions: the hub queue and each client
	// buffer drop on overflow, so a terminal event like trip_finished can vanish
	// with neither side knowing. A client that sees seq jump can tell it missed
	// something and refetch GET /trip/:id instead of sitting on a stale state.
	Seq int64 `json:"seq,omitempty"`
}

// client is a WebSocket connection subscribed to one or more trip IDs. userID is set when using auth (for disconnect logging).
type client struct {
	hub     *Hub
	conn    *websocket.Conn
	send    chan []byte
	tripIDs map[string]struct{}
	userID  int64
	mu      sync.Mutex
}

// Hub holds registered clients and broadcasts events by trip_id.
type Hub struct {
	mu            sync.RWMutex
	tripToClients map[string]map[*client]struct{}
	clients       map[*client]struct{}
	register      chan *client
	unregister    chan *client
	broadcast     chan broadcastReq

	seqMu sync.Mutex
	seq   map[string]int64 // trip_id -> last sequence number
}

type broadcastReq struct {
	tripID string
	event  Event
}

// NewHub creates a new Hub. Call Run() to start the loop.
func NewHub() *Hub {
	return &Hub{
		tripToClients: make(map[string]map[*client]struct{}),
		clients:       make(map[*client]struct{}),
		seq:           make(map[string]int64),
		register:      make(chan *client),
		unregister:    make(chan *client),
		broadcast:     make(chan broadcastReq, 64),
	}
}

// Run runs the hub loop (blocking). Run in a goroutine.
//
// Each case delegates to a method that unlocks with defer. This loop is run
// under supervision, so a panic while the lock is held must not leave it held —
// the restarted loop would deadlock on its first Lock and every WebSocket
// registration would block forever.
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.addClient(c)
		case c := <-h.unregister:
			h.removeClient(c)
		case req := <-h.broadcast:
			h.fanOut(req)
		}
	}
}

func (h *Hub) addClient(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	for tripID := range c.tripIDs {
		if h.tripToClients[tripID] == nil {
			h.tripToClients[tripID] = make(map[*client]struct{})
		}
		h.tripToClients[tripID][c] = struct{}{}
	}
}

func (h *Hub) removeClient(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	for tripID := range c.tripIDs {
		delete(h.tripToClients[tripID], c)
		if len(h.tripToClients[tripID]) == 0 {
			delete(h.tripToClients, tripID)
		}
	}
	close(c.send)
}

func (h *Hub) fanOut(req broadcastReq) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	clients := h.tripToClients[req.tripID]
	if clients == nil {
		return
	}
	msg, err := json.Marshal(req.event)
	if err != nil {
		return
	}
	for c := range clients {
		select {
		case c.send <- msg:
		default:
		}
	}
}

// NotifySessionRevoked sends {"type":"session_revoked"} to every connection of
// the given user and closes them shortly after (single-session driver login:
// the old device must learn it was signed out without waiting for its next
// HTTP call). Best-effort like all hub delivery.
func (h *Hub) NotifySessionRevoked(userID int64) {
	if h == nil || userID <= 0 {
		return
	}
	msg, err := json.Marshal(Event{Type: "session_revoked", EmittedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*client, 0, 2)
	for c := range h.clients {
		if c.userID == userID {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		select {
		case c.send <- msg:
		default:
		}
		if c.conn != nil {
			conn := c.conn
			time.AfterFunc(500*time.Millisecond, func() { _ = conn.Close() })
		}
	}
}

// BroadcastToTrip sends an event to all clients subscribed to the trip. Sets EmittedAt if empty.
func (h *Hub) BroadcastToTrip(tripID string, event Event) {
	if tripID == "" {
		return
	}
	event.TripID = tripID
	if event.EmittedAt == "" {
		event.EmittedAt = time.Now().UTC().Format(time.RFC3339)
	}
	event.Seq = h.nextSeq(tripID)
	select {
	case h.broadcast <- broadcastReq{tripID: tripID, event: event}:
	default:
		log.Printf("ws: broadcast queue full for trip %s", tripID)
	}
}

// Upgrader upgrades HTTP to WebSocket.
var Upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// ConfigureCheckOrigin sets Upgrader.CheckOrigin from an allowlist.
//
// Allowed:
//   - empty Origin (native Flutter/iOS/Android clients that omit it)
//   - exact match in allowedOrigins
//   - "*" in allowedOrigins → allow any Origin (parity with HTTP CORS *)
//   - loopback Origin (localhost, 127.0.0.0/8, ::1), used by packaged
//     Flutter/Capacitor WebViews whose local asset server opens the socket
//   - Origin whose host matches the request Host (Flutter/Dart WebSocket
//     stacks often set Origin to the API base URL itself; WEBAPP_URL alone
//     would reject those and yield 403 "request origin not allowed")
//
// Rejected origins are logged with the Origin value so misconfigured
// WS_ALLOWED_ORIGINS is diagnosable from production tails.
func ConfigureCheckOrigin(allowedOrigins []string) {
	allow := make(map[string]struct{}, len(allowedOrigins))
	allowAll := false
	for _, o := range allowedOrigins {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "" {
			continue
		}
		if o == "*" {
			allowAll = true
			continue
		}
		allow[o] = struct{}{}
	}
	Upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
		if origin == "" {
			return true
		}
		if allowAll {
			return true
		}
		if _, ok := allow[origin]; ok {
			return true
		}
		if isLoopbackOrigin(origin) {
			return true
		}
		if originHostMatchesRequest(origin, r.Host) {
			return true
		}
		log.Printf("ws: CheckOrigin rejected origin=%q host=%q path=%s allowlist_size=%d",
			origin, r.Host, r.URL.Path, len(allow))
		return false
	}
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originHostMatchesRequest reports whether origin's host equals the request host
// (case-insensitive). Accepts either bare hostname or host:port on either side.
func originHostMatchesRequest(origin, reqHost string) bool {
	reqHost = strings.TrimSpace(reqHost)
	if origin == "" || reqHost == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	originHost := u.Hostname()
	reqHostname := reqHost
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		reqHostname = h
	}
	// Strip brackets from IPv6 hostnames for EqualFold.
	originHost = strings.TrimPrefix(strings.TrimSuffix(originHost, "]"), "[")
	reqHostname = strings.TrimPrefix(strings.TrimSuffix(reqHostname, "]"), "[")
	if originHost != "" && strings.EqualFold(originHost, reqHostname) {
		return true
	}
	return strings.EqualFold(u.Host, reqHost)
}

// nextSeq returns the next per-trip sequence number.
func (h *Hub) nextSeq(tripID string) int64 {
	h.seqMu.Lock()
	defer h.seqMu.Unlock()
	if h.seq == nil {
		h.seq = make(map[string]int64)
	}
	h.seq[tripID]++
	return h.seq[tripID]
}

// ForgetTrip drops the sequence counter for a finished trip so the map does not
// grow for the life of the process.
func (h *Hub) ForgetTrip(tripID string) {
	h.seqMu.Lock()
	defer h.seqMu.Unlock()
	delete(h.seq, tripID)
}

// lastSeq reports the current sequence number for a trip (0 if unknown).
// Exposed for tests and diagnostics.
func (h *Hub) lastSeq(tripID string) int64 {
	h.seqMu.Lock()
	defer h.seqMu.Unlock()
	return h.seq[tripID]
}
