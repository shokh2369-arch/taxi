package handlers

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/accounting"
	"taxi-mvp/internal/auth"
)

// DriverReferralStatus returns referral progress for the authenticated driver when they were referred by another driver.
func DriverReferralStatus(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		st, err := accounting.GetReferredDriverReferralStatus(c.Request.Context(), db, u.UserID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load referral status"})
			return
		}
		c.JSON(http.StatusOK, st)
	}
}
