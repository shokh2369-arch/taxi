package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/services"
)

// RegisterRiderAccountRoutes mounts the account-deletion endpoint for the
// native rider app (Google Play requires an in-app deletion path):
//
//	DELETE /v1/rider/account    Authorization: Bearer <access_token>
//
// 200 {"ok":true} — PII erased/anonymized, every session revoked.
// 409 invalid_state — a trip is WAITING/ARRIVED/STARTED; finish or cancel first.
func RegisterRiderAccountRoutes(r *gin.Engine, db *sql.DB, riderAuthSvc *services.RiderAuthService) {
	if r == nil || db == nil || riderAuthSvc == nil {
		return
	}
	bearer := RequireRiderBearerAuth(riderAuthSvc, db)
	g := r.Group("/v1/rider")
	g.Use(bearer)
	g.DELETE("/account", riderAppDeleteAccount(riderAuthSvc))
}

func riderAppDeleteAccount(svc *services.RiderAuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		if err := svc.DeleteAccount(c.Request.Context(), uid); err != nil {
			if errors.Is(err, services.ErrRiderAccountActiveTrip) {
				writeRiderAPIError(c, http.StatusConflict, "invalid_state",
					"Faol safar mavjud. Avval safarni yakunlang yoki bekor qiling.")
				return
			}
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error",
				"Texnik xatolik. Birozdan keyin qayta urinib ko‘ring.")
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
