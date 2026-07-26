package services

import (
	"testing"
	"time"
)

func TestRiderWSTicket_IssueRedeemOneTime(t *testing.T) {
	svc := NewRiderWSTicketService()
	ticket, ttl, err := svc.Issue(42)
	if err != nil || ticket == "" || ttl <= 0 {
		t.Fatalf("Issue: %v ticket=%q ttl=%d", err, ticket, ttl)
	}
	uid, ok := svc.Redeem(ticket)
	if !ok || uid != 42 {
		t.Fatalf("Redeem: ok=%v uid=%d want 42", ok, uid)
	}
	// One-time: the second redeem must fail.
	if _, ok := svc.Redeem(ticket); ok {
		t.Fatalf("ticket redeemed twice")
	}
}

func TestRiderWSTicket_Expiry(t *testing.T) {
	svc := NewRiderWSTicketService()
	base := time.Now()
	svc.now = func() time.Time { return base }
	ticket, _, err := svc.Issue(7)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return base.Add(61 * time.Second) }
	if _, ok := svc.Redeem(ticket); ok {
		t.Fatalf("expired ticket must not redeem")
	}
}

func TestRiderWSTicket_NilSafe(t *testing.T) {
	var svc *RiderWSTicketService
	if _, ok := svc.Redeem("x"); ok {
		t.Fatalf("nil service must not redeem")
	}
	if _, _, err := svc.Issue(1); err == nil {
		t.Fatalf("nil service must not issue")
	}
}

func TestRiderWSTicket_UnknownTicket(t *testing.T) {
	svc := NewRiderWSTicketService()
	if _, ok := svc.Redeem("never-issued"); ok {
		t.Fatalf("unknown ticket must not redeem")
	}
}
