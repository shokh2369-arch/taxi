package ws

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthPresenceSummary(t *testing.T) {
	t.Run("bare request reports all absent", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/ws/driver-dispatch", nil)
		got := authPresenceSummary(req)
		want := "authorization_header=0 x_driver_session_header=0 init_data_header=0 x_driver_id_header=0 ws_protocol_header=0 access_token_query=0 init_data_query=0 driver_id_query=0"
		if got != want {
			t.Fatalf("summary = %q, want %q", got, want)
		}
	})

	t.Run("flags presence without leaking values", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/ws/driver-dispatch?driver_id=4&access_token=secret-token", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		req.Header.Set("X-Driver-Id", "4")
		got := authPresenceSummary(req)
		want := "authorization_header=1 x_driver_session_header=0 init_data_header=0 x_driver_id_header=1 ws_protocol_header=0 access_token_query=1 init_data_query=0 driver_id_query=1"
		if got != want {
			t.Fatalf("summary = %q, want %q", got, want)
		}
		if strings.Contains(got, "secret-token") {
			t.Fatalf("summary leaked token value: %q", got)
		}
	})

	t.Run("x_driver_id query counts as driver_id_query", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/ws/driver-dispatch?x_driver_id=4", nil)
		got := authPresenceSummary(req)
		want := "authorization_header=0 x_driver_session_header=0 init_data_header=0 x_driver_id_header=0 ws_protocol_header=0 access_token_query=0 init_data_query=0 driver_id_query=1"
		if got != want {
			t.Fatalf("summary = %q, want %q", got, want)
		}
	})
}
