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
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, X-Telegram-Init-Data, X-Driver-Id")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d", w.Code)
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
