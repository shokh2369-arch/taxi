package services

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/ws"
)

// DispatchOfferVisibleSeconds is how long a per-driver offer stays in GET /driver/available-requests
// after request_notifications.created_at (minimum visibility; offers also end when the request is no longer PENDING).
func DispatchOfferVisibleSeconds(cfg *config.Config) int {
	if cfg != nil && cfg.DispatchOfferVisibleSeconds > 0 {
		return cfg.DispatchOfferVisibleSeconds
	}
	return 90
}

// LogDriverQueueOfferRemoved logs why an offer disappeared from a driver's app queue.
func LogDriverQueueOfferRemoved(requestID string, driverUserID int64, reason string) {
	log.Printf("dispatch_queue: removed request=%s driver=%d reason=%s", requestID, driverUserID, reason)
}

// CloseDriverQueueOffersExcept marks SENT notifications closed for every driver except exceptDriverUserID.
// Returns how many rows were closed. emitDispatchChanged triggers a WebSocket poke when any row closes.
func CloseDriverQueueOffersExcept(ctx context.Context, db *sql.DB, requestID string, exceptDriverUserID int64, reason string, emitDispatchChanged bool) int {
	if db == nil || requestID == "" {
		return 0
	}
	rows, err := db.QueryContext(ctx, `
		SELECT driver_user_id FROM request_notifications
		WHERE request_id = ?1 AND status = ?2 AND driver_user_id != ?3`,
		requestID, domain.NotificationStatusSent, exceptDriverUserID)
	if err != nil {
		return 0
	}
	defer rows.Close()

	var driverIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		driverIDs = append(driverIDs, id)
	}
	if len(driverIDs) == 0 {
		return 0
	}
	for _, driverID := range driverIDs {
		res, err := db.ExecContext(ctx, `
			UPDATE request_notifications SET status = ?1
			WHERE request_id = ?2 AND driver_user_id = ?3 AND status = ?4`,
			domain.NotificationStatusTimeout, requestID, driverID, domain.NotificationStatusSent)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			LogDriverQueueOfferRemoved(requestID, driverID, reason)
		}
	}
	if emitDispatchChanged {
		ws.NotifyDispatchChangedBurst()
	}
	return len(driverIDs)
}

// CloseAllDriverQueueOffers marks every SENT notification for requestID closed.
func CloseAllDriverQueueOffers(ctx context.Context, db *sql.DB, requestID string, reason string, emitDispatchChanged bool) int {
	return CloseDriverQueueOffersExcept(ctx, db, requestID, 0, reason, emitDispatchChanged)
}

// ExpireStaleDriverQueueOffers closes SENT rows when the parent ride request is no longer dispatchable
// or the per-driver offer is older than DispatchOfferVisibleSeconds (created_at visibility window).
func ExpireStaleDriverQueueOffers(ctx context.Context, db *sql.DB, cfg *config.Config) int {
	if db == nil {
		return 0
	}
	visibleModifier := fmt.Sprintf("-%d seconds", DispatchOfferVisibleSeconds(cfg))
	rows, err := db.QueryContext(ctx, `
		SELECT n.request_id, n.driver_user_id
		FROM request_notifications n
		JOIN ride_requests r ON r.id = n.request_id
		WHERE n.status = ?1
		  AND (r.status != ?2 OR r.expires_at <= datetime('now')
		       OR n.created_at <= datetime('now', ?3))`,
		domain.NotificationStatusSent, domain.RequestStatusPending, visibleModifier)
	if err != nil {
		return 0
	}
	defer rows.Close()

	type pair struct {
		requestID string
		driverID  int64
	}
	var toClose []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.requestID, &p.driverID); err != nil {
			continue
		}
		toClose = append(toClose, p)
	}
	if len(toClose) == 0 {
		return 0
	}
	closed := 0
	for _, p := range toClose {
		res, err := db.ExecContext(ctx, `
			UPDATE request_notifications SET status = ?1
			WHERE request_id = ?2 AND driver_user_id = ?3 AND status = ?4`,
			domain.NotificationStatusTimeout, p.requestID, p.driverID, domain.NotificationStatusSent)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			reason := "expired"
			LogDriverQueueOfferRemoved(p.requestID, p.driverID, reason)
			closed++
		}
	}
	if closed > 0 {
		ws.NotifyDispatchChangedBurst()
	}
	return closed
}
