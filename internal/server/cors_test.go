package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCORSPreflight_AllowsDriverAndMiniAppHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(corsMiddleware())
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://mini.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, X-Telegram-Init-Data, X-Driver-Id")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://mini.example" {
		t.Fatalf("Allow-Origin = %q, want echoed Origin", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Allow-Credentials = %q, want true", got)
	}
	allowH := w.Header().Get("Access-Control-Allow-Headers")
	for _, want := range []string{"Authorization", "X-Telegram-Init-Data", "X-Driver-Id", "Content-Type"} {
		if !strings.Contains(allowH, want) {
			t.Fatalf("Allow-Headers %q missing %q", allowH, want)
		}
	}
	methods := w.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(methods, "GET") || !strings.Contains(methods, "POST") {
		t.Fatalf("Allow-Methods = %q", methods)
	}
}

func TestCORSPreflight_FlutterWebLocalhost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(corsMiddleware())
	r.POST("/v1/rider/auth/request-code", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/rider/auth/request-code", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8101")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, authorization, x-requested-with")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8101" {
		t.Fatalf("Allow-Origin = %q", got)
	}
	allowH := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers"))
	for _, want := range []string{"content-type", "authorization", "x-requested-with"} {
		if !strings.Contains(allowH, want) {
			t.Fatalf("Allow-Headers %q missing %q", allowH, want)
		}
	}

	// Actual POST must also echo Origin so BrowserClient can read the body.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/rider/auth/request-code", strings.NewReader(`{"phone":"+998990708446"}`))
	req2.Header.Set("Origin", "http://127.0.0.1:8101")
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("POST status = %d", w2.Code)
	}
	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8101" {
		t.Fatalf("POST Allow-Origin = %q", got)
	}
}
