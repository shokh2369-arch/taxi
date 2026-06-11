package auth

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/domain"
)

// ActionSource identifies who initiated a driver-facing state transition.
type ActionSource int

const (
	// ActionSourceUnknown — no source on context; driver Telegram notifications behave as before.
	ActionSourceUnknown ActionSource = iota
	// ActionSourceTelegram — driver bot callback, live location, or Mini App with valid init data.
	ActionSourceTelegram
	// ActionSourceHTTPApp — native driver app authenticated HTTP API (not Telegram WebApp).
	ActionSourceHTTPApp
	// ActionSourceInternal — cron, admin, or system jobs (default when not HTTP/Telegram).
	ActionSourceInternal
)

type actionSourceKey struct{}

// WithActionSource attaches an action source to ctx.
func WithActionSource(ctx context.Context, src ActionSource) context.Context {
	return context.WithValue(ctx, actionSourceKey{}, src)
}

// ActionSourceFromContext returns the action source, or ActionSourceUnknown when unset.
func ActionSourceFromContext(ctx context.Context) ActionSource {
	if ctx == nil {
		return ActionSourceUnknown
	}
	v := ctx.Value(actionSourceKey{})
	if v == nil {
		return ActionSourceUnknown
	}
	src, _ := v.(ActionSource)
	return src
}

// SkipDriverTelegramNotify is true when the transition was initiated from the native HTTP driver app.
func SkipDriverTelegramNotify(ctx context.Context) bool {
	return ActionSourceFromContext(ctx) == ActionSourceHTTPApp
}

// SkipRiderTelegramNotify is true when the transition was initiated from the native HTTP rider app.
func SkipRiderTelegramNotify(ctx context.Context) bool {
	return ActionSourceFromContext(ctx) == ActionSourceHTTPApp
}

// ContextWithDetachedActionSource copies the action source onto a new background-safe context (e.g. goroutines).
func ContextWithDetachedActionSource(parent context.Context) context.Context {
	src := ActionSourceFromContext(parent)
	if src == ActionSourceUnknown {
		return context.Background()
	}
	return WithActionSource(context.Background(), src)
}

// HeaderDriverSession is sent by the native driver app after SMS login (future/alternate to X-Driver-Id).
const HeaderDriverSession = "X-Driver-Session"

// DetectDriverActionSource classifies an authenticated driver HTTP request.
// Call after driver auth middleware. Does not change Telegram initData or bot flows.
func DetectDriverActionSource(c *gin.Context) ActionSource {
	if c == nil || c.Request == nil {
		return ActionSourceUnknown
	}
	path := c.Request.URL.Path
	if path == "/driver/location/app" {
		return ActionSourceHTTPApp
	}
	initData := strings.TrimSpace(c.GetHeader(HeaderInitData))
	if initData == "" {
		initData = strings.TrimSpace(c.Query("init_data"))
	}
	if initData != "" {
		return ActionSourceTelegram
	}
	if strings.TrimSpace(c.GetHeader(HeaderDriverSession)) != "" {
		return ActionSourceHTTPApp
	}
	if strings.TrimSpace(c.GetHeader(HeaderDriverID)) != "" {
		return ActionSourceHTTPApp
	}
	if u := UserFromContext(c.Request.Context()); u != nil && u.Role == domain.RoleDriver && u.TelegramUserID == 0 {
		return ActionSourceHTTPApp
	}
	return ActionSourceTelegram
}

// InjectDriverActionSource sets ActionSource on the request context for driver HTTP routes.
func InjectDriverActionSource() gin.HandlerFunc {
	return func(c *gin.Context) {
		src := DetectDriverActionSource(c)
		c.Request = c.Request.WithContext(WithActionSource(c.Request.Context(), src))
		c.Next()
	}
}
