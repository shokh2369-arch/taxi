package handlers

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/legal"
)

type legalAcceptBody struct {
	Version int `json:"version"` // ignored; server accepts only active versions
}

func clientIP(c *gin.Context) string {
	if xff := strings.TrimSpace(c.GetHeader("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host := c.Request.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

// LegalActiveDocuments returns active legal texts for the authenticated user's role.
func LegalActiveDocuments(db *sql.DB) gin.HandlerFunc {
	svc := legal.NewService(db)
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		ctx := c.Request.Context()
		var types []string
		switch u.Role {
		case domain.RoleDriver:
			types = []string{legal.DocDriverTerms, legal.DocPrivacyPolicyDriver}
		case domain.RoleRider:
			types = []string{legal.DocUserTerms, legal.DocPrivacyPolicyUser}
		default:
			c.JSON(http.StatusForbidden, gin.H{"error": "unknown role"})
			return
		}
		docs, err := svc.ActiveDocuments(ctx, types)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load documents"})
			return
		}
		out := make([]gin.H, 0, len(types))
		for _, t := range types {
			if d, ok := docs[t]; ok {
				out = append(out, gin.H{"document_type": t, "version": d.Version, "content": d.Content})
			}
		}
		c.JSON(http.StatusOK, gin.H{"documents": out})
	}
}

// LegalAccept records acceptance of all active documents for the user's role (active versions only; body.version ignored).
func LegalAccept(db *sql.DB) gin.HandlerFunc {
	svc := legal.NewService(db)
	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var body legalAcceptBody
		_ = c.ShouldBindJSON(&body)
		ctx := c.Request.Context()
		ip := clientIP(c)
		ua := c.GetHeader("User-Agent")
		var types []string
		switch u.Role {
		case domain.RoleDriver:
			types = []string{legal.DocDriverTerms, legal.DocPrivacyPolicyDriver}
		case domain.RoleRider:
			types = []string{legal.DocUserTerms, legal.DocPrivacyPolicyUser}
		default:
			c.JSON(http.StatusForbidden, gin.H{"error": "unknown role"})
			return
		}
		if err := svc.AcceptActiveForTypes(ctx, u.UserID, types, ip, ua); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "accept failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
