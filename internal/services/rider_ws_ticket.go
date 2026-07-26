package services

import (
	"errors"
	"sync"
	"time"
)

var errNilTicketService = errors.New("rider ws tickets: service unavailable")

// RiderWSTicketService issues short-lived one-time tickets that web builds use
// to open GET /ws without putting a long-lived JWT in the URL (query strings
// land in edge logs). POST /v1/rider/ws-ticket (Bearer) returns a ticket; the
// WS handler redeems it exactly once.
//
// In-memory on purpose: tickets live seconds, and the service runs as a single
// instance (see render.yaml). A restart drops outstanding tickets — the client
// simply requests a new one, same as after expiry.
type RiderWSTicketService struct {
	mu  sync.Mutex
	m   map[string]wsTicket
	ttl time.Duration
	now func() time.Time
}

type wsTicket struct {
	userID  int64
	expires time.Time
}

// NewRiderWSTicketService returns a ticket service with a 60s ticket TTL.
func NewRiderWSTicketService() *RiderWSTicketService {
	return &RiderWSTicketService{
		m:   make(map[string]wsTicket),
		ttl: 60 * time.Second,
		now: time.Now,
	}
}

// Issue stores a fresh one-time ticket for the rider and returns it with its
// TTL in seconds.
func (s *RiderWSTicketService) Issue(userID int64) (string, int, error) {
	if s == nil {
		return "", 0, errNilTicketService
	}
	ticket, err := generateOpaqueToken(32)
	if err != nil {
		return "", 0, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Lazy purge keeps the map bounded without a background goroutine.
	for k, v := range s.m {
		if now.After(v.expires) {
			delete(s.m, k)
		}
	}
	s.m[ticket] = wsTicket{userID: userID, expires: now.Add(s.ttl)}
	return ticket, int(s.ttl.Seconds()), nil
}

// Redeem consumes a ticket: it is deleted on first use whether or not it is
// still valid, so a leaked URL can be replayed at most zero times after the
// legitimate connect.
func (s *RiderWSTicketService) Redeem(ticket string) (int64, bool) {
	// Nil-receiver safe: server wiring stores this in an interface field, and a
	// typed nil must behave as "no ticket auth", not panic.
	if s == nil || ticket == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[ticket]
	if !ok {
		return 0, false
	}
	delete(s.m, ticket)
	if s.now().After(v.expires) {
		return 0, false
	}
	return v.userID, true
}
