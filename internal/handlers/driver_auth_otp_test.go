package handlers

import "testing"

func TestNormalizePhoneDigits(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"+998901234567", "998901234567"},
		{"998901234567", "998901234567"},
		{"901234567", "998901234567"},
		{"  +998 90 123 45 67 ", "998901234567"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizePhoneDigits(tt.in); got != tt.want {
			t.Errorf("normalizePhoneDigits(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
