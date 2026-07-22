package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"taxi-mvp/internal/auth"
	"taxi-mvp/internal/domain"
)

// tripHistoryTimeToRFC3339 normalizes SQLite / legacy timestamps for Flutter parsers.
func tripHistoryTimeToRFC3339(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z07:00",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

// DriverTrips returns a read-only list of terminal trips for the authenticated driver
// (native app "Safarlar tarixi"). Auth matches GET /driver/available-requests.
//
// Query: limit (default 50, max 100), offset (default 0, non-negative).
func DriverTrips(db *sql.DB) gin.HandlerFunc {
	const qWithCommission = `
		SELECT
			t.id,
			t.status,
			t.started_at,
			t.finished_at,
			t.cancelled_at,
			t.fare_amount,
			COALESCE((
				SELECT SUM(ABS(p.amount))
				FROM payments p
				WHERE p.trip_id = t.id AND p.type = 'commission'
			), 0) AS commission_som
		FROM trips t
		WHERE t.driver_user_id = ?1
		  AND t.status IN ('FINISHED','CANCELLED','CANCELLED_BY_DRIVER','CANCELLED_BY_RIDER')
		ORDER BY datetime(COALESCE(t.finished_at, t.cancelled_at, t.started_at, '1970-01-01')) DESC, t.id DESC
		LIMIT ?2 OFFSET ?3`

	const qNoCommission = `
		SELECT
			t.id,
			t.status,
			t.started_at,
			t.finished_at,
			t.cancelled_at,
			t.fare_amount,
			0 AS commission_som
		FROM trips t
		WHERE t.driver_user_id = ?1
		  AND t.status IN ('FINISHED','CANCELLED','CANCELLED_BY_DRIVER','CANCELLED_BY_RIDER')
		ORDER BY datetime(COALESCE(t.finished_at, t.cancelled_at, t.started_at, '1970-01-01')) DESC, t.id DESC
		LIMIT ?2 OFFSET ?3`

	return func(c *gin.Context) {
		u := auth.UserFromContext(c.Request.Context())
		if u == nil || u.Role != domain.RoleDriver {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "driver auth required"})
			return
		}
		ctx := c.Request.Context()
		driverID := u.UserID

		limit := 50
		if ls := strings.TrimSpace(c.Query("limit")); ls != "" {
			if n, err := strconv.Atoi(ls); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 100 {
			limit = 100
		}
		offset := 0
		if os := strings.TrimSpace(c.Query("offset")); os != "" {
			if n, err := strconv.Atoi(os); err == nil && n >= 0 {
				offset = n
			}
		}

		rows, err := db.QueryContext(ctx, qWithCommission, driverID, limit, offset)
		if err != nil {
			errStr := strings.ToLower(err.Error())
			if (strings.Contains(errStr, "no such column") && strings.Contains(errStr, "trip_id")) ||
				strings.Contains(errStr, "no such table: payments") {
				rows, err = db.QueryContext(ctx, qNoCommission, driverID, limit, offset)
			}
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		defer rows.Close()

		var trips []gin.H
		for rows.Next() {
			var id, status string
			var startedAt, finishedAt, cancelledAt sql.NullString
			// fare_amount is nullable in production; scanning into int64 silently drops the row.
			var fareAmount sql.NullInt64
			var commissionSom int64
			if err := rows.Scan(&id, &status, &startedAt, &finishedAt, &cancelledAt, &fareAmount, &commissionSom); err != nil {
				continue
			}

			item := gin.H{
				"trip_id":   id,
				"id":        id,
				"uuid":      id,
				"status":    status,
				"fare_som":  fareAmount.Int64,
				"price":     fareAmount.Int64,
				"total_som": fareAmount.Int64,
			}
			if startedAt.Valid && strings.TrimSpace(startedAt.String) != "" {
				if ts := tripHistoryTimeToRFC3339(startedAt.String); ts != "" {
					item["started_at"] = ts
				}
			}
			if finishedAt.Valid && strings.TrimSpace(finishedAt.String) != "" {
				if ts := tripHistoryTimeToRFC3339(finishedAt.String); ts != "" {
					item["finished_at"] = ts
				}
			}
			if cancelledAt.Valid && strings.TrimSpace(cancelledAt.String) != "" {
				if ts := tripHistoryTimeToRFC3339(cancelledAt.String); ts != "" {
					item["cancelled_at"] = ts
				}
			}
			if commissionSom > 0 {
				item["commission_som"] = commissionSom
			}
			trips = append(trips, item)
		}
		if err := rows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"trips": trips})
	}
}
