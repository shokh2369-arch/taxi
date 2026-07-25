package ws

import (
	"net/http/httptest"
	"testing"
)

func TestRiderAccessTokenFromRequest(t *testing.T) {
	t.Run("authorization header wins", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/ws?access_token=query-token", nil)
		req.Header.Set("Authorization", "Bearer header-token")
		if got := riderAccessTokenFromRequest(req); got != "header-token" {
			t.Fatalf("token = %q, want header-token", got)
		}
	})

	t.Run("query token works without debug auth", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/ws?access_token=query-token", nil)
		if got := riderAccessTokenFromRequest(req); got != "query-token" {
			t.Fatalf("token = %q, want query-token", got)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/ws", nil)
		if got := riderAccessTokenFromRequest(req); got != "" {
			t.Fatalf("token = %q, want empty", got)
		}
	})
}
