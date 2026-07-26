package handlers

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/services"
)

// RiderAuthDeps groups the rider native-auth dependencies used by the
// handlers. Constructed once in cmd/app/main and passed to RegisterRiderAuthRoutes.
type RiderAuthDeps struct {
	Service *services.RiderAuthService
}

// riderAuthErrorEnvelope is the shape of every error response returned by the
// rider-auth endpoints:
//
//	{ "error": { "code": "phone_not_registered", "message": "Iltimos, ..." } }
//
// We keep it consistent across the four endpoints so the Flutter rider app can
// branch on the stable `code` field while displaying `message` to the user.
type riderAuthErrorEnvelope struct {
	Error riderAuthErrorBody `json:"error"`
}

type riderAuthErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeRiderAuthError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, riderAuthErrorEnvelope{
		Error: riderAuthErrorBody{Code: code, Message: message},
	})
}

// RegisterRiderAuthRoutes mounts the four endpoints that the Flutter rider
// app calls during sign-in.
//
//	POST /v1/rider/auth/request-code  { "phone": "+998..." }
//	POST /v1/rider/auth/verify-code   { "phone", "code" }
//	POST /v1/rider/auth/refresh       { "refresh_token" }
//	POST /v1/rider/auth/logout        Authorization: Bearer <access_token>
func RegisterRiderAuthRoutes(r *gin.Engine, deps RiderAuthDeps) {
	if r == nil || deps.Service == nil {
		return
	}
	g := r.Group("/v1/rider/auth")
	g.POST("/request-code", riderAuthRequestCode(deps.Service))
	g.POST("/verify-code", riderAuthVerifyCode(deps.Service))
	g.POST("/refresh", riderAuthRefresh(deps.Service))
	g.POST("/logout", riderAuthLogout(deps.Service))
}

// RegisterRiderAppLegalRoutes mounts legal document fetch + accept for the
// native rider app (Authorization: Bearer access_token). Response bodies match
// GET /legal/active and POST /legal/accept (Mini App / Telegram initData).
func RegisterRiderAppLegalRoutes(r *gin.Engine, db *sql.DB, riderAuthSvc *services.RiderAuthService) {
	if r == nil || db == nil || riderAuthSvc == nil {
		return
	}
	bearer := RequireRiderBearerAuth(riderAuthSvc, db)
	g := r.Group("/v1/rider/legal")
	g.Use(bearer)
	g.GET("/active", LegalActiveDocuments(db))
	g.POST("/accept", LegalAccept(db))
}

type riderAuthRequestCodeBody struct {
	Phone string `json:"phone"`
}

type riderAuthVerifyCodeBody struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

type riderAuthRefreshBody struct {
	RefreshToken string `json:"refresh_token"`
}

func riderAuthRequestCode(svc *services.RiderAuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Eager entry log — fires BEFORE body parse / DB lookup / bot send so
		// that "is the request even reaching this handler?" can be answered
		// from a single Render deploy log line. We never log the raw phone or
		// any future OTP — only a length and a client-ip hint.
		log.Printf("rider_auth http=request-code status=received body_len=%d ip=%s",
			c.Request.ContentLength, c.ClientIP())

		var body riderAuthRequestCodeBody
		if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Phone) == "" {
			log.Printf("rider_auth http=request-code status=rejected reason=invalid_phone")
			writeRiderAuthError(c, http.StatusBadRequest, "invalid_phone",
				"Telefon raqami noto‘g‘ri. Iltimos, +998XXXXXXXXX ko‘rinishida kiriting.")
			return
		}
		_, err := svc.RequestCode(c.Request.Context(), body.Phone)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"ok": true})
			return
		}
		mapRiderAuthError(c, err)
	}
}

func riderAuthVerifyCode(svc *services.RiderAuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		log.Printf("rider_auth http=verify-code status=received body_len=%d ip=%s",
			c.Request.ContentLength, c.ClientIP())

		var body riderAuthVerifyCodeBody
		if err := c.ShouldBindJSON(&body); err != nil {
			log.Printf("rider_auth http=verify-code status=rejected reason=invalid_body")
			writeRiderAuthError(c, http.StatusBadRequest, "invalid_body",
				"Yuborilgan ma‘lumot noto‘g‘ri.")
			return
		}
		tokens, err := svc.VerifyCode(c.Request.Context(), body.Phone, body.Code)
		if err == nil {
			c.JSON(http.StatusOK, tokens)
			return
		}
		mapRiderAuthError(c, err)
	}
}

func riderAuthRefresh(svc *services.RiderAuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body riderAuthRefreshBody
		if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.RefreshToken) == "" {
			writeRiderAuthError(c, http.StatusBadRequest, "invalid_refresh_token",
				"Refresh token noto‘g‘ri. Qaytadan tizimga kiring.")
			return
		}
		tokens, err := svc.Refresh(c.Request.Context(), body.RefreshToken)
		if err == nil {
			c.JSON(http.StatusOK, tokens)
			return
		}
		writeRiderAuthError(c, http.StatusUnauthorized, "invalid_refresh_token",
			"Refresh token eskirgan yoki bekor qilingan. Qaytadan tizimga kiring.")
	}
}

func riderAuthLogout(svc *services.RiderAuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			writeRiderAuthError(c, http.StatusUnauthorized, "unauthorized",
				"Avtorizatsiya talab qilinadi.")
			return
		}
		userID, err := svc.VerifyAccessToken(token)
		if err != nil {
			writeRiderAuthError(c, http.StatusUnauthorized, "unauthorized",
				"Sessiya muddati tugagan. Qaytadan tizimga kiring.")
			return
		}
		if err := svc.Logout(c.Request.Context(), userID); err != nil {
			writeRiderAuthError(c, http.StatusInternalServerError, "internal_error",
				"Texnik xatolik. Birozdan keyin qayta urinib ko‘ring.")
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
func bearerToken(c *gin.Context) string {
	h := strings.TrimSpace(c.GetHeader("Authorization"))
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// mapRiderAuthError converts service-layer errors into the JSON envelope
// expected by the Flutter rider app. The Uzbek messages match the style of
// the existing legal/driver flows.
func mapRiderAuthError(c *gin.Context, err error) {
	var recent *services.RiderAuthCodeRecentError
	if errors.As(err, &recent) {
		secs := int(recent.RetryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		// Retry-After header is the primary signal; retry_after_seconds in the
		// body is the fallback for web clients, where CORS may hide the header.
		c.Writer.Header().Set("Retry-After", itoa(secs))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error": gin.H{
				"code":                "code_recently_sent",
				"message":             "Kod yaqinda yuborildi. " + itoa(secs) + " soniyadan so‘ng qayta urinib ko‘ring.",
				"retry_after_seconds": secs,
			},
		})
		return
	}
	switch {
	case errors.Is(err, services.ErrRiderAuthInvalidPhone):
		writeRiderAuthError(c, http.StatusBadRequest, "invalid_phone",
			"Telefon raqami noto‘g‘ri. Iltimos, +998XXXXXXXXX ko‘rinishida kiriting.")
	// "phone not in users" and "users row exists but telegram_id is null"
	// both collapse to the same 409 telegram_not_linked. The rider app shows
	// one clear instruction to the user and we don't reveal whether the
	// phone is otherwise known to the system (per the spec).
	case errors.Is(err, services.ErrRiderAuthPhoneNotFound),
		errors.Is(err, services.ErrRiderAuthTelegramNotLink):
		writeRiderAuthError(c, http.StatusConflict, "telegram_not_linked",
			"Iltimos, avval Telegramdagi YettiQanot rider botiga /start bering va kontakt yuboring.")
	case errors.Is(err, services.ErrRiderAuthTooManyCodes):
		writeRiderAuthError(c, http.StatusTooManyRequests, "too_many_codes",
			"Juda ko‘p kod so‘rovi. 1 soatdan so‘ng qayta urinib ko‘ring.")
	case errors.Is(err, services.ErrRiderAuthBotBlocked):
		writeRiderAuthError(c, http.StatusConflict, "bot_blocked",
			"Bot bloklangan. Iltimos, YettiQanot rider botida /start bosing va qaytadan urinib ko‘ring.")
	case errors.Is(err, services.ErrRiderAuthSendFailed):
		writeRiderAuthError(c, http.StatusBadGateway, "telegram_send_failed",
			"Telegram orqali kodni yuborib bo‘lmadi. Birozdan keyin qayta urinib ko‘ring.")
	case errors.Is(err, services.ErrRiderAuthInvalidCode):
		writeRiderAuthError(c, http.StatusBadRequest, "invalid_code",
			"Kod noto‘g‘ri yoki muddati tugagan. Qaytadan kod so‘rang.")
	// 400, not 429: the app contract (docs/RIDER_CLIENT.md §2) groups
	// too_many_attempts with invalid_code — the code is consumed and the fix
	// is requesting a new one, not waiting.
	case errors.Is(err, services.ErrRiderAuthTooManyAttempts):
		writeRiderAuthError(c, http.StatusBadRequest, "too_many_attempts",
			"Kod ko‘p marta noto‘g‘ri kiritildi. Iltimos, yangi kod so‘rang.")
	default:
		writeRiderAuthError(c, http.StatusInternalServerError, "internal_error",
			"Texnik xatolik. Birozdan keyin qayta urinib ko‘ring.")
	}
}

// itoa avoids importing strconv just for the Retry-After header.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
