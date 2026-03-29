package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/legal"
)

// RegisterAdminLegalRoutes mounts GET/HEAD /admin/legal/* on the API (dashboard legal monitoring, issues, documents).
// Call from server startup with the same *sql.DB as the app; safe no-op if r or db is nil.
func RegisterAdminLegalRoutes(r *gin.Engine, db *sql.DB) {
	if r == nil || db == nil {
		return
	}
	g := r.Group("/admin")
	registerAdminLegalRoutes(g, db)
}

// registerAdminLegalRoutes mounts /admin/legal/* under an existing /admin RouterGroup.
func registerAdminLegalRoutes(g *gin.RouterGroup, db *sql.DB) {
	if g == nil || db == nil {
		return
	}
	h := &adminLegalHTTP{db: db}
	lg := g.Group("/legal")
	{
		lg.GET("", h.monitoring)
		lg.HEAD("", h.monitoringHead)
		lg.GET("/monitoring", h.monitoring)
		lg.HEAD("/monitoring", h.monitoringHead)
		lg.GET("/status", h.monitoring)
		lg.HEAD("/status", h.monitoringHead)
		lg.GET("/summary", h.monitoring)
		lg.GET("/health", h.monitoring)
		lg.HEAD("/health", h.monitoringHead)
		lg.GET("/issues", h.issues)
		lg.HEAD("/issues", h.issuesHead)
		lg.GET("/problems", h.issues)
		lg.HEAD("/problems", h.issuesHead)
		lg.GET("/documents", h.documents)
		lg.HEAD("/documents", h.documentsHead)
		lg.GET("/users/:user_id/acceptances", h.userAcceptances)
		lg.HEAD("/users/:user_id/acceptances", h.userAcceptancesHead)
	}
}

func (h *adminLegalHTTP) monitoringHead(c *gin.Context) {
	c.Status(http.StatusOK)
}

func (h *adminLegalHTTP) issuesHead(c *gin.Context) {
	c.Status(http.StatusOK)
}

func (h *adminLegalHTTP) documentsHead(c *gin.Context) {
	c.Status(http.StatusOK)
}

func (h *adminLegalHTTP) userAcceptancesHead(c *gin.Context) {
	c.Status(http.StatusOK)
}

type adminLegalHTTP struct {
	db *sql.DB
}

const adminLegalJoin = `INNER JOIN legal_documents ld ON ld.document_type = la.document_type AND ld.version = la.version AND ld.is_active = 1`

func (h *adminLegalHTTP) monitoring(c *gin.Context) {
	ctx := c.Request.Context()
	svc := legal.NewService(h.db)
	types := []string{legal.DocDriverTerms, legal.DocUserTerms, legal.DocPrivacyPolicy}
	docs, err := svc.ActiveDocuments(ctx, types)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": "legal tables or query failed", "detail": err.Error()})
		return
	}
	docList := make([]gin.H, 0, len(types))
	for _, t := range types {
		if d, ok := docs[t]; ok {
			prev := d.Content
			if len(prev) > 240 {
				prev = prev[:240] + "…"
			}
			docList = append(docList, gin.H{
				"document_type":   t,
				"version":         d.Version,
				"content_preview": strings.TrimSpace(prev),
			})
		}
	}
	var driversTotal, driversOK, ridersTotal, ridersOK int64
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM drivers`).Scan(&driversTotal)
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM drivers d WHERE 3 = (
			SELECT COUNT(*) FROM legal_acceptances la `+adminLegalJoin+`
			WHERE la.user_id = d.user_id
			AND la.document_type IN ('driver_terms','user_terms','privacy_policy')
		)`).Scan(&driversOK)
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'rider'`).Scan(&ridersTotal)
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users u WHERE u.role = 'rider' AND 2 = (
			SELECT COUNT(*) FROM legal_acceptances la `+adminLegalJoin+`
			WHERE la.user_id = u.id
			AND la.document_type IN ('user_terms','privacy_policy')
		)`).Scan(&ridersOK)

	c.JSON(http.StatusOK, gin.H{
		"ok":                 true,
		"enabled":            true,
		"service":            "taxi-mvp",
		"active_documents":   docList,
		"active_document_count": len(docList),
		"counts": gin.H{
			"drivers_total":           driversTotal,
			"drivers_fully_compliant": driversOK,
			"riders_total":            ridersTotal,
			"riders_fully_compliant":  ridersOK,
			"drivers_missing_legal":   driversTotal - driversOK,
			"riders_missing_legal":    ridersTotal - ridersOK,
		},
	})
}

func (h *adminLegalHTTP) issues(c *gin.Context) {
	ctx := c.Request.Context()
	rows, err := h.db.QueryContext(ctx, `
		SELECT d.user_id AS id, 'driver' AS role FROM drivers d WHERE 3 > (
			SELECT COUNT(*) FROM legal_acceptances la `+adminLegalJoin+`
			WHERE la.user_id = d.user_id
			AND la.document_type IN ('driver_terms','user_terms','privacy_policy')
		)
		UNION ALL
		SELECT u.id, 'rider' FROM users u WHERE u.role = 'rider' AND 2 > (
			SELECT COUNT(*) FROM legal_acceptances la `+adminLegalJoin+`
			WHERE la.user_id = u.id
			AND la.document_type IN ('user_terms','privacy_policy')
		)
		ORDER BY id DESC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()
	type issueRow struct {
		ID   int64  `json:"user_id"`
		Role string `json:"role"`
	}
	var list []issueRow
	for rows.Next() {
		var r issueRow
		if err := rows.Scan(&r.ID, &r.Role); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "issues": list, "count": len(list)})
}

func (h *adminLegalHTTP) documents(c *gin.Context) {
	ctx := c.Request.Context()
	rows, err := h.db.QueryContext(ctx, `
		SELECT document_type, version, is_active,
		       CASE WHEN LENGTH(content) > 400 THEN SUBSTR(content, 1, 400) || '…' ELSE content END
		FROM legal_documents
		ORDER BY document_type, version DESC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()
	var out []gin.H
	for rows.Next() {
		var dt string
		var ver, active int
		var body string
		if err := rows.Scan(&dt, &ver, &active, &body); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		out = append(out, gin.H{
			"document_type": dt,
			"version":       ver,
			"is_active":     active == 1,
			"content":       body,
		})
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "documents": out})
}

func (h *adminLegalHTTP) userAcceptances(c *gin.Context) {
	ctx := c.Request.Context()
	idStr := c.Param("user_id")
	uid, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || uid <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
		return
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT la.document_type, la.version, la.accepted_at,
		       EXISTS(SELECT 1 FROM legal_documents ld
		              WHERE ld.document_type = la.document_type AND ld.version = la.version AND ld.is_active = 1) AS matches_active
		FROM legal_acceptances la
		WHERE la.user_id = ?1
		ORDER BY la.accepted_at DESC`, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()
	var out []gin.H
	for rows.Next() {
		var dt string
		var ver int
		var at string
		var matchActive int
		if err := rows.Scan(&dt, &ver, &at, &matchActive); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		out = append(out, gin.H{
			"document_type":          dt,
			"version":                ver,
			"accepted_at":            at,
			"matches_active_version": matchActive != 0,
		})
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "user_id": uid, "acceptances": out})
}
