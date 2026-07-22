package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RequireAdminAPIToken requires Authorization: Bearer <token> matching the configured ADMIN_API_TOKEN.
// Callers must not register admin routes when token is empty.
func RequireAdminAPIToken(expectedToken string) gin.HandlerFunc {
	expectedToken = strings.TrimSpace(expectedToken)
	return func(c *gin.Context) {
		if expectedToken == "" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		raw := strings.TrimSpace(c.GetHeader("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(raw, prefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin authentication required"})
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(raw, prefix))
		if !secureStringEqual(got, expectedToken) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin authentication required"})
			return
		}
		c.Next()
	}
}

func secureStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
