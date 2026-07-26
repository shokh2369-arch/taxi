package ws

import (
	"fmt"
	"net/http"
	"strings"

	"taxi-mvp/internal/auth"
)

// authPresenceSummary reports which credential kinds a rejected WS upgrade request
// carried — presence flags only, never values (tokens must not reach logs; the
// http_request access log omits query strings for the same reason).
// It distinguishes "client sent no credential" from "client sent one we rejected"
// when diagnosing production 401s, e.g. a driver app that still connects with only
// a driver id after ENABLE_DRIVER_ID_HEADER was turned off.
func authPresenceSummary(r *http.Request) string {
	q := r.URL.Query()
	has := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	return fmt.Sprintf(
		"authorization_header=%d x_driver_session_header=%d init_data_header=%d x_driver_id_header=%d ws_protocol_header=%d access_token_query=%d init_data_query=%d driver_id_query=%d",
		has(strings.TrimSpace(r.Header.Get("Authorization")) != ""),
		has(strings.TrimSpace(r.Header.Get(auth.HeaderDriverSession)) != ""),
		has(strings.TrimSpace(r.Header.Get(headerInitData)) != ""),
		has(strings.TrimSpace(r.Header.Get(auth.HeaderDriverID)) != ""),
		has(strings.TrimSpace(r.Header.Get("Sec-WebSocket-Protocol")) != ""),
		has(strings.TrimSpace(q.Get("access_token")) != ""),
		has(strings.TrimSpace(q.Get("init_data")) != ""),
		has(strings.TrimSpace(q.Get("driver_id")) != "" || strings.TrimSpace(q.Get("x_driver_id")) != ""),
	)
}
