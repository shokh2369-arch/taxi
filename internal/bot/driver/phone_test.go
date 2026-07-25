package driver

import (
	"testing"

	"taxi-mvp/internal/domain"
)

// The drivers.phone unique index exists to stop one person farming referral
// promo across many accounts. That only holds if the field is a real phone
// number: previously it took arbitrary text, so "1", "2", "3"… were all valid
// distinct "phone numbers" and the constraint was free to bypass.
func TestNormalizeUzbekDriverPhone(t *testing.T) {
	valid := map[string]string{
		"+998901234567":       "+998901234567",
		"998901234567":        "+998901234567",
		"901234567":           "+998901234567",
		" +998 90 123-45-67 ": "+998901234567",
		"(998) 33 123 45 67":  "+998331234567",
	}
	for in, want := range valid {
		got, ok := normalizeUzbekDriverPhone(in)
		if !ok || got != want {
			t.Errorf("normalizeUzbekDriverPhone(%q) = (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}

	rejected := []string{
		"", "1", "a", "x2", "12345", "0000",
		"+1234567890",   // not Uzbek
		"9989012345678", // too long
		"99812345678",   // too short
		"+998201234567", // invalid operator code
		"select 1",      // free text
	}
	for _, in := range rejected {
		if got, ok := normalizeUzbekDriverPhone(in); ok {
			t.Errorf("normalizeUzbekDriverPhone(%q) accepted as %q; a non-phone must be rejected or the unique index is meaningless", in, got)
		}
	}
}

// The bot must expose the whole trip lifecycle, not just a Mini App link: a
// driver whose map does not open could previously accept a ride and then had no
// way to arrive, start, finish or cancel it.
func TestTripControlKeyboard_CoversEveryStage(t *testing.T) {
	cases := []struct {
		status    string
		wantData  []string
		notWanted []string
	}{
		{domain.TripStatusWaiting, []string{cbTripArrived + "t1", cbTripStart + "t1", cbTripCancel + "t1"}, nil},
		{domain.TripStatusArrived, []string{cbTripStart + "t1", cbTripCancel + "t1"}, []string{cbTripArrived + "t1"}},
		// Mid-ride the rider is already in the car, so cancelling is not offered.
		{domain.TripStatusStarted, []string{cbTripFinish + "t1"}, []string{cbTripCancel + "t1"}},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			kb := tripControlKeyboard("t1", tc.status)
			var got []string
			for _, row := range kb.InlineKeyboard {
				for _, b := range row {
					if b.CallbackData != nil {
						got = append(got, *b.CallbackData)
					}
				}
			}
			has := func(s string) bool {
				for _, g := range got {
					if g == s {
						return true
					}
				}
				return false
			}
			for _, want := range tc.wantData {
				if !has(want) {
					t.Errorf("status %s missing control %q; buttons=%v", tc.status, want, got)
				}
			}
			for _, no := range tc.notWanted {
				if has(no) {
					t.Errorf("status %s should not offer %q; buttons=%v", tc.status, no, got)
				}
			}
		})
	}
}

func TestTripActionErrorText_IsActionable(t *testing.T) {
	for _, err := range []error{
		domain.ErrLiveLocationInactive,
		domain.ErrDriverLocationStale,
		domain.ErrTooFarFromPickup,
		domain.ErrInvalidTransition,
		domain.ErrTripNotFound,
	} {
		if got := tripActionErrorText(err); got == "" {
			t.Errorf("no message for %v", err)
		}
	}
}
