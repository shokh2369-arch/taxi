package auth

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/domain"
)

// XDriverIDResolveStatus is the outcome of resolving X-Driver-Id (shared by HTTP middleware and WebSocket).
type XDriverIDResolveStatus int

const (
	// XDriverIDOK — internal users.id resolved and driver is approved.
	XDriverIDOK XDriverIDResolveStatus = iota
	XDriverIDBadFormat
	XDriverIDNotFound
	XDriverIDNotApproved
)

// ResolveDriverUserForXDriverID resolves the internal driver user_id (users.id) from the raw header value.
// Lookup order: (1) drivers.user_id = parsed integer, (2) users.telegram_id = parsed integer with a drivers row.
// Only verification_status = 'approved' succeeds with XDriverIDOK (standalone app matches dispatch eligibility).
// Does not change Telegram initData or bot flows.
func ResolveDriverUserForXDriverID(ctx context.Context, db *sql.DB, raw string) (userID int64, st XDriverIDResolveStatus) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, XDriverIDNotFound
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, XDriverIDBadFormat
	}
	var uid int64
	var ver sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT d.user_id, d.verification_status FROM drivers d WHERE d.user_id = ?1`, n).Scan(&uid, &ver)
	if err == nil {
		if strings.TrimSpace(ver.String) != "approved" {
			return uid, XDriverIDNotApproved
		}
		return uid, XDriverIDOK
	}
	if err != sql.ErrNoRows {
		return 0, XDriverIDNotFound
	}
	err = db.QueryRowContext(ctx, `
		SELECT d.user_id, d.verification_status
		FROM drivers d
		INNER JOIN users u ON u.id = d.user_id
		WHERE u.telegram_id = ?1`, n).Scan(&uid, &ver)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, XDriverIDNotFound
		}
		return 0, XDriverIDNotFound
	}
	if strings.TrimSpace(ver.String) != "approved" {
		return uid, XDriverIDNotApproved
	}
	return uid, XDriverIDOK
}

// LogDriverHeaderDebug logs only booleans when DRIVER_AUTH_DEBUG is on; never logs header values or IDs.
func LogDriverHeaderDebug(debug bool, path string, headerPathEnabled, headerPresent bool) {
	if !debug {
		return
	}
	log.Printf("driver_auth_debug path=%s driver_header_path_enabled=%v x_driver_id_header_present=%v", path, headerPathEnabled, headerPresent)
}

func abortXDriverID(c *gin.Context, st XDriverIDResolveStatus) {
	switch st {
	case XDriverIDBadFormat:
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid X-Driver-Id (digits only: internal user id or Telegram user id)"})
	case XDriverIDNotFound:
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unknown driver id (use internal user id from admin/users, or Telegram user id)"})
	case XDriverIDNotApproved:
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "driver not approved"})
	default:
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "driver authentication failed"})
	}
}

// DriverIDHeaderMiddlewareOpts configures X-Driver-Id middleware (HTTP).
type DriverIDHeaderMiddlewareOpts struct {
	Enable bool
	Debug  bool
}

// TryDriverIDHeader sets the driver from X-Driver-Id when Enable is true and the header is present and valid.
// When Enable is false, the header is ignored (opt-in via ENABLE_DRIVER_ID_HEADER=false). When Enable is true and the header
// is absent, the request continues so RequireDriverAuth can use initData.
// When Enable is true and the header is present but invalid / unknown / not approved, responds and aborts (distinct errors).
func TryDriverIDHeader(db *sql.DB, opts DriverIDHeaderMiddlewareOpts) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		headerPresent := strings.TrimSpace(c.GetHeader(HeaderDriverID)) != ""
		LogDriverHeaderDebug(opts.Debug, path, opts.Enable, headerPresent)

		if !opts.Enable {
			c.Next()
			return
		}
		raw := strings.TrimSpace(c.GetHeader(HeaderDriverID))
		if raw == "" {
			c.Next()
			return
		}
		uid, st := ResolveDriverUserForXDriverID(c.Request.Context(), db, raw)
		if st != XDriverIDOK {
			abortXDriverID(c, st)
			return
		}
		c.Request = c.Request.WithContext(WithUser(c.Request.Context(), &User{
			UserID:         uid,
			TelegramUserID: 0,
			Role:           domain.RoleDriver,
		}))
		c.Next()
	}
}
