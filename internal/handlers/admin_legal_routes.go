package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/legal"
)

// RegisterAdminLegalRoutes mounts GET/HEAD .../admin/legal/* on the API.
// Registers under /admin, /api/admin, /api/v1/admin, and /v1/admin so dashboards that probe different API prefixes all work.
func RegisterAdminLegalRoutes(r *gin.Engine, db *sql.DB) {
	if r == nil || db == nil {
		return
	}
	for _, base := range []string{"/admin", "/api/admin", "/api/v1/admin", "/v1/admin"} {
		g := r.Group(base)
		registerAdminLegalRoutes(g, db)
	}
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
		lg.GET("/acceptances", h.allAcceptances)
		lg.HEAD("/acceptances", h.allAcceptancesHead)
		lg.GET("/missing", h.missingLegal)
		lg.HEAD("/missing", h.missingLegalHead)
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

func (h *adminLegalHTTP) allAcceptancesHead(c *gin.Context) {
	c.Status(http.StatusOK)
}

func (h *adminLegalHTTP) missingLegalHead(c *gin.Context) {
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

// missingLegal lists users missing required acceptances for active document versions.
// Query actor_type: driver | rider | all (default all). Matches dashboard probes, e.g. ?actor_type=driver
func (h *adminLegalHTTP) missingLegal(c *gin.Context) {
	ctx := c.Request.Context()
	at := strings.TrimSpace(strings.ToLower(c.Query("actor_type")))
	switch at {
	case "", "all":
		at = "all"
	case "driver", "drivers":
		at = "driver"
	case "rider", "riders":
		at = "rider"
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"ok":    false,
			"error": "invalid actor_type; use driver, rider, or all",
		})
		return
	}

	type missRow struct {
		UserID int64  `json:"user_id"`
		Role   string `json:"role"`
	}
	var list []missRow

	if at == "all" || at == "driver" {
		rows, qerr := h.db.QueryContext(ctx, `
			SELECT d.user_id AS id, 'driver' AS role FROM drivers d WHERE 3 > (
				SELECT COUNT(*) FROM legal_acceptances la `+adminLegalJoin+`
				WHERE la.user_id = d.user_id
				AND la.document_type IN ('driver_terms','user_terms','privacy_policy')
			)
			ORDER BY id DESC`)
		if qerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": qerr.Error()})
			return
		}
		defer rows.Close()
		for rows.Next() {
			var r missRow
			if err := rows.Scan(&r.UserID, &r.Role); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
				return
			}
			list = append(list, r)
		}
		if err := rows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
	}

	if at == "all" || at == "rider" {
		rows, qerr := h.db.QueryContext(ctx, `
			SELECT u.id, 'rider' AS role FROM users u WHERE u.role = 'rider' AND 2 > (
				SELECT COUNT(*) FROM legal_acceptances la `+adminLegalJoin+`
				WHERE la.user_id = u.id
				AND la.document_type IN ('user_terms','privacy_policy')
			)
			ORDER BY id DESC`)
		if qerr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": qerr.Error()})
			return
		}
		defer rows.Close()
		for rows.Next() {
			var r missRow
			if err := rows.Scan(&r.UserID, &r.Role); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
				return
			}
			list = append(list, r)
		}
		if err := rows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"actor_type": at,
		"missing":    list,
		"count":      len(list),
	})
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

// allAcceptances returns all rows from legal_acceptances (newest first) for admin dashboards.
// Query: limit (default 2000, max 10000), offset (default 0).
func (h *adminLegalHTTP) allAcceptances(c *gin.Context) {
	ctx := c.Request.Context()
	limit := 2000
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 10000 {
		limit = 10000
	}
	offset := 0
	if s := c.Query("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			offset = n
		}
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT la.user_id,
		       la.document_type,
		       la.version,
		       la.accepted_at,
		       EXISTS(SELECT 1 FROM legal_documents ld
		              WHERE ld.document_type = la.document_type AND ld.version = la.version AND ld.is_active = 1) AS matches_active,
		       COALESCE(u.role, '') AS role,
		       COALESCE(u.name, '') AS user_name
		FROM legal_acceptances la
		LEFT JOIN users u ON u.id = la.user_id
		ORDER BY la.accepted_at DESC, la.user_id DESC
		LIMIT ?1 OFFSET ?2`, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()
	var list []gin.H
	for rows.Next() {
		var uid int64
		var dt, at, role, name string
		var ver, match int
		if err := rows.Scan(&uid, &dt, &ver, &at, &match, &role, &name); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
			return
		}
		list = append(list, gin.H{
			"user_id":                uid,
			"document_type":          dt,
			"version":                ver,
			"accepted_at":            at,
			"matches_active_version": match != 0,
			"role":                   role,
			"user_name":              name,
		})
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "acceptances": list, "count": len(list), "limit": limit, "offset": offset})
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
