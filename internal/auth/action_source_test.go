package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/domain"
)

func TestDetectDriverActionSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name string
		path string
		hdr  map[string]string
		user *User
		want ActionSource
	}{
		{
			name: "location app is always http app",
			path: "/driver/location/app",
			hdr:  map[string]string{HeaderInitData: "stub"},
			want: ActionSourceHTTPApp,
		},
		{
			name: "init data is telegram",
			path: "/trip/arrived",
			hdr:  map[string]string{HeaderInitData: "valid-init"},
			want: ActionSourceTelegram,
		},
		{
			name: "driver id without init is http app",
			path: "/trip/start",
			hdr:  map[string]string{HeaderDriverID: "42"},
			want: ActionSourceHTTPApp,
		},
		{
			name: "driver session is http app",
			path: "/trip/finish",
			hdr:  map[string]string{HeaderDriverSession: "sess-abc"},
			want: ActionSourceHTTPApp,
		},
		{
			name: "native user context without headers",
			path: "/driver/offline",
			user: &User{UserID: 7, TelegramUserID: 0, Role: domain.RoleDriver},
			want: ActionSourceHTTPApp,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			for k, v := range tc.hdr {
				c.Request.Header.Set(k, v)
			}
			ctx := c.Request.Context()
			if tc.user != nil {
				ctx = WithUser(ctx, tc.user)
			}
			c.Request = c.Request.WithContext(ctx)
			if got := DetectDriverActionSource(c); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSkipDriverTelegramNotify(t *testing.T) {
	if SkipDriverTelegramNotify(context.Background()) {
		t.Fatal("unknown source should not skip")
	}
	ctx := WithActionSource(context.Background(), ActionSourceHTTPApp)
	if !SkipDriverTelegramNotify(ctx) {
		t.Fatal("http app should skip driver telegram")
	}
	ctx = WithActionSource(context.Background(), ActionSourceTelegram)
	if SkipDriverTelegramNotify(ctx) {
		t.Fatal("telegram should not skip")
	}
}

func TestSkipRiderTelegramNotify(t *testing.T) {
	if SkipRiderTelegramNotify(context.Background()) {
		t.Fatal("unknown source should not skip")
	}
	ctx := WithActionSource(context.Background(), ActionSourceHTTPApp)
	if !SkipRiderTelegramNotify(ctx) {
		t.Fatal("http app should skip rider telegram")
	}
	ctx = WithActionSource(context.Background(), ActionSourceTelegram)
	if SkipRiderTelegramNotify(ctx) {
		t.Fatal("telegram should not skip")
	}
}
