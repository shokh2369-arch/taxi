package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"taxi-mvp/internal/services"
)

// RiderNotificationDeps wires GET /v1/rider/notifications (Bearer auth).
type RiderNotificationDeps struct {
	DB           *sql.DB
	RiderAuthSvc *services.RiderAuthService
}

// RegisterRiderNotificationRoutes mounts Bearer-authenticated notification list
// under /v1/rider/notifications for the native Flutter rider app.
func RegisterRiderNotificationRoutes(r *gin.Engine, deps RiderNotificationDeps) {
	if r == nil || deps.DB == nil || deps.RiderAuthSvc == nil {
		return
	}
	bearer := RequireRiderBearerAuth(deps.RiderAuthSvc, deps.DB)
	g := r.Group("/v1/rider")
	g.Use(bearer)
	g.GET("/notifications", riderAppListNotifications(deps))
}

func riderAppListNotifications(deps RiderNotificationDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := riderUserID(c)
		if !ok {
			return
		}
		ctx := c.Request.Context()

		limit := 50
		if s := strings.TrimSpace(c.Query("limit")); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				switch {
				case n < 1:
					limit = 50
				case n > 100:
					limit = 100
				default:
					limit = n
				}
			}
		}

		rows, err := deps.DB.QueryContext(ctx, `
			SELECT id, title, body, created_at,
			       cloudinary_secure_url,
			       cloudinary_public_id,
			       media_type,
			       width,
			       height,
			       format
			FROM (
				SELECT n.id AS id,
				       n.title AS title,
				       n.body AS body,
				       n.created_at AS created_at,
				       NULL AS cloudinary_secure_url,
				       NULL AS cloudinary_public_id,
				       NULL AS media_type,
				       NULL AS width,
				       NULL AS height,
				       NULL AS format
				FROM rider_app_notifications n
				WHERE n.rider_user_id = ?1
				  AND TRIM(n.body) != ''
				UNION ALL
				SELECT b.id AS id,
				       b.title AS title,
				       b.body AS body,
				       b.created_at AS created_at,
				       b.cloudinary_secure_url AS cloudinary_secure_url,
				       b.cloudinary_public_id AS cloudinary_public_id,
				       b.media_type AS media_type,
				       b.width AS width,
				       b.height AS height,
				       b.format AS format
				FROM broadcast_posts b
				WHERE b.status = 'published'
				  AND COALESCE(b.audience, 'all_riders') = 'all_riders'
				  AND (
				    TRIM(b.body) != ''
				    OR COALESCE(TRIM(b.cloudinary_secure_url), '') != ''
				  )
			)
			ORDER BY datetime(created_at) DESC, id DESC
			LIMIT ?2`, uid, limit)
		if err != nil {
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
			return
		}
		defer rows.Close()

		var list []map[string]any
		for rows.Next() {
			var id, body, createdAt string
			var title sql.NullString
			var secureURL, publicID, mediaType, format sql.NullString
			var width, height sql.NullInt64
			if err := rows.Scan(&id, &title, &body, &createdAt, &secureURL, &publicID, &mediaType, &width, &height, &format); err != nil {
				writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
				return
			}
			body = strings.TrimSpace(body)
			itemTitle := ""
			if title.Valid {
				itemTitle = strings.TrimSpace(title.String)
			}
			var media *riderNotificationMedia
			if secureURL.Valid {
				w, h := 0, 0
				if width.Valid {
					w = int(width.Int64)
				}
				if height.Valid {
					h = int(height.Int64)
				}
				media = buildRiderNotificationMedia(
					secureURL.String,
					publicID.String,
					mediaType.String,
					format.String,
					w, h,
				)
			}
			if body == "" && media == nil {
				continue
			}
			list = append(list, riderNotificationItemJSON(
				strings.TrimSpace(id),
				itemTitle,
				body,
				normalizeRiderNotificationTime(createdAt),
				media,
			))
		}
		if err := rows.Err(); err != nil {
			writeRiderAPIError(c, http.StatusInternalServerError, "internal_error", "Texnik xatolik.")
			return
		}
		if list == nil {
			list = []map[string]any{}
		}
		c.JSON(http.StatusOK, gin.H{"notifications": list})
	}
}

func normalizeRiderNotificationTime(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05Z07:00",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}
