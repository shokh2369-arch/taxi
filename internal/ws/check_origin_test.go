package ws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigureCheckOrigin_EmptyOriginAllowed(t *testing.T) {
	ConfigureCheckOrigin([]string{"https://mini.example"})
	req := httptest.NewRequest(http.MethodGet, "/ws/driver-dispatch", nil)
	req.Host = "api.example.com"
	if !Upgrader.CheckOrigin(req) {
		t.Fatal("empty Origin must be allowed (native clients)")
	}
}

func TestConfigureCheckOrigin_AllowlistMatch(t *testing.T) {
	ConfigureCheckOrigin([]string{"https://mini.example"})
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Origin", "https://mini.example")
	req.Host = "api.example.com"
	if !Upgrader.CheckOrigin(req) {
		t.Fatal("allowlisted Origin must be accepted")
	}
}

func TestConfigureCheckOrigin_SameHostAsAPI(t *testing.T) {
	// Flutter/Dart often sets Origin to the WebSocket URL's own origin.
	ConfigureCheckOrigin([]string{"https://mini.example"})
	req := httptest.NewRequest(http.MethodGet, "/ws/driver-dispatch", nil)
	req.Header.Set("Origin", "https://api.example.com")
	req.Host = "api.example.com"
	if !Upgrader.CheckOrigin(req) {
		t.Fatal("Origin matching request Host must be accepted")
	}
}

func TestConfigureCheckOrigin_RejectForeign(t *testing.T) {
	ConfigureCheckOrigin([]string{"https://mini.example"})
	req := httptest.NewRequest(http.MethodGet, "/ws/driver-dispatch", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Host = "api.example.com"
	if Upgrader.CheckOrigin(req) {
		t.Fatal("foreign Origin must be rejected")
	}
}

func TestConfigureCheckOrigin_StarAllowsAny(t *testing.T) {
	ConfigureCheckOrigin([]string{"*"})
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Origin", "https://anything.example")
	req.Host = "api.example.com"
	if !Upgrader.CheckOrigin(req) {
		t.Fatal("* must allow any Origin")
	}
}

func TestOriginHostMatchesRequest(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"https://api.example.com", "api.example.com", true},
		{"https://api.example.com", "api.example.com:443", true},
		{"https://API.EXAMPLE.COM", "api.example.com", true},
		{"https://api.example.com:443", "api.example.com", true},
		{"https://other.example.com", "api.example.com", false},
		{"", "api.example.com", false},
		{"https://api.example.com", "", false},
	}
	for _, tc := range cases {
		origin := strings.TrimRight(tc.origin, "/")
		if got := originHostMatchesRequest(origin, tc.host); got != tc.want {
			t.Errorf("originHostMatchesRequest(%q, %q)=%v want %v", tc.origin, tc.host, got, tc.want)
		}
	}
}
