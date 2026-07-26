package handlers

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/services"
)

// RegisterRiderWSTicketRoutes mounts the one-time WebSocket ticket endpoint:
//
//	POST /v1/rider/ws-ticket    Authorization: Bearer <access_token>
//	  200 {"ticket":"...","expires_in":60}
//
// Web builds call this right before opening GET /ws?trip_id=...&ticket=... so
// the long-lived JWT never appears in a URL. ?access_token= keeps working for
// clients that have not migrated.
func RegisterRiderWSTicketRoutes(r *gin.Engine, db *sql.DB, riderAuthSvc *services.RiderAuthService, tickets *services.RiderWSTicketService) {
	if r == nil || db == nil || riderAuthSvc == nil || tickets == nil {
		return
	}
	bearer := RequireRiderBearerAuth(riderAuthSvc, db)
	g := r.Group("/v1/rider")
	g.Use(bearer)
	g.POST("/ws-ticket", riderAppIssueWSTicket(tickets))
}

func riderAppIssueWSTicket(tickets *services.RiderWSTicketService) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		ticket, ttl, err := tickets.Issue(uid)
		if err != nil {
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error",
				"Texnik xatolik. Birozdan keyin qayta urinib ko‘ring.")
			return
		}
		c.JSON(http.StatusOK, gin.H{"ticket": ticket, "expires_in": ttl})
	}
}
