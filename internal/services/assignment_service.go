package services

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/legal"
	"taxi-mvp/internal/ws"
)

const assignmentLogErrMaxChars = 200

func assignmentTrunc(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max < 4 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func assignmentErrStr(err error) string {
	if err == nil {
		return ""
	}
	return assignmentTrunc(err.Error(), assignmentLogErrMaxChars)
}

// AssignmentService assigns requests to drivers and runs the expiry worker.
type AssignmentService struct {
	db        *sql.DB
	riderBot  *tgbotapi.BotAPI
	driverBot *tgbotapi.BotAPI
	cfg       *config.Config
}

// NewAssignmentService returns an AssignmentService.
func NewAssignmentService(db *sql.DB, riderBot, driverBot *tgbotapi.BotAPI, cfg *config.Config) *AssignmentService {
	return &AssignmentService{db: db, riderBot: riderBot, driverBot: driverBot, cfg: cfg}
}

// TryAssign atomically assigns the request to the driver. Only one driver can accept (race-safe).
// Returns (true, tripID, nil) if assigned; (false, "", nil) if another driver already accepted.
func (s *AssignmentService) TryAssign(ctx context.Context, requestID string, driverUserID int64) (assigned bool, tripID string, err error) {
	if !legal.NewService(s.db).DriverHasActiveLegal(ctx, driverUserID) {
		return false, "", fmt.Errorf("assignment: driver %d missing active legal acceptances", driverUserID)
	}
	// Single transaction: ASSIGNED status, ACCEPTED notification, and trip insert commit or roll back
	// together, so a failed trip insert cannot leave the request stuck in ASSIGNED without a trip.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.ExecContext(ctx, `
		UPDATE ride_requests
		SET status = ?1, assigned_driver_user_id = ?2, assigned_at = datetime('now')
		WHERE id = ?3 AND status = ?4 AND expires_at > datetime('now')`,
		domain.RequestStatusAssigned, driverUserID, requestID, domain.RequestStatusPending)
	if err != nil {
		return false, "", err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return false, "", nil // another driver already accepted or request expired
	}
	var riderUserID int64
	err = tx.QueryRowContext(ctx, `SELECT rider_user_id FROM ride_requests WHERE id = ?1`, requestID).Scan(&riderUserID)
	if err != nil {
		return false, "", err
	}
	// Mark this driver's notification as ACCEPTED (for dispatch log)
	_, _ = tx.ExecContext(ctx, `UPDATE request_notifications SET status = ?1 WHERE request_id = ?2 AND driver_user_id = ?3`,
		domain.NotificationStatusAccepted, requestID, driverUserID)

	tripID = uuid.New().String()
	// The compare-and-swap above stops two drivers taking the same request, but
	// nothing stopped one driver taking two different requests: both offers pass
	// the "no active trip" filter at offer time, and accepting them touches
	// different ride_requests rows so the CAS never conflicts. The driver app
	// shows only one assigned trip (LIMIT 1), so the second rider waits forever
	// for a driver who is not coming. Re-check inside the transaction.
	insertRes, err := tx.ExecContext(ctx, `
		INSERT INTO trips (id, request_id, driver_user_id, rider_user_id, status)
		SELECT ?1, ?2, ?3, ?4, ?5
		WHERE NOT EXISTS (
			SELECT 1 FROM trips
			WHERE driver_user_id = ?3 AND status IN (?6, ?7, ?8)
		)`,
		tripID, requestID, driverUserID, riderUserID, domain.TripStatusWaiting,
		domain.TripStatusWaiting, domain.TripStatusArrived, domain.TripStatusStarted)
	if err != nil {
		return false, "", err
	}
	if inserted, _ := insertRes.RowsAffected(); inserted == 0 {
		// Driver already has an active trip. Roll back so the request returns to
		// PENDING and stays available to other drivers.
		log.Printf("assignment_service: driver %d already has an active trip; request %s left pending", driverUserID, requestID)
		return false, "", nil
	}
	if err := tx.Commit(); err != nil {
		return false, "", err
	}
	committed = true
	log.Printf("dispatch_audit: request=%s accepted_by=%d trip_id=%s", requestID, driverUserID, tripID)
	CloseDriverQueueOffersExcept(ctx, s.db, requestID, driverUserID, "accepted_by_other", true)

	var riderTelegramID int64
	err = s.db.QueryRowContext(ctx, `SELECT telegram_id FROM users WHERE id = ?1`, riderUserID).Scan(&riderTelegramID)
	if err == nil && riderTelegramID != 0 && !ShouldSkipRiderTripTelegramNotify(ctx, s.db, riderUserID) {
		chatID := riderTelegramID
		// Include driver info (phone first) so rider can contact the driver
		var driverPhone, carType, color, plate string
		var userPhone sql.NullString
		_ = s.db.QueryRowContext(ctx, `
			SELECT COALESCE(d.phone,''), COALESCE(d.car_type,''), COALESCE(d.color,''), COALESCE(d.plate,''), u.phone
			FROM drivers d JOIN users u ON u.id = d.user_id WHERE d.user_id = ?1`, driverUserID).
			Scan(&driverPhone, &carType, &color, &plate, &userPhone)
		phone := strings.TrimSpace(driverPhone)
		if phone == "" && userPhone.Valid && strings.TrimSpace(userPhone.String) != "" {
			phone = strings.TrimSpace(userPhone.String)
		}
		body := "🚗 Ҳайдовчи топилди!\n\nСизни қуйидаги ҳайдовчи олиб кетади:\n"
		if phone != "" {
			body += "📞 Телефон: " + phone + "\n"
		}
		if carType != "" {
			body += "🚗 " + carType
			if color != "" {
				body += ", " + color
			}
			body += "\n"
		} else if color != "" {
			body += "🚗 " + color + "\n"
		}
		if plate != "" {
			body += "🔢 " + plate + "\n"
		}
		msg := tgbotapi.NewMessage(chatID, body)
		if s.cfg.RiderMapURL != "" {
			riderMapURL := strings.TrimSuffix(s.cfg.RiderMapURL, "/") + "?trip_id=" + tripID
			msg.ReplyMarkup = riderMapWebAppKeyboard("📍 Ҳайдовчини кузатиш", riderMapURL)
		}
		if _, err := s.riderBot.Send(msg); err != nil {
			log.Printf("assignment_service: notify rider: %v", assignmentErrStr(err))
		}
		// Reply keyboard for trip-active state: Haydovchini kuzatish, Bekor qilish
		riderTripActiveKeyboard := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButton("📍 Ҳайдовчини кузатиш"),
				tgbotapi.NewKeyboardButton("❌ Бекор қилиш"),
			),
		)
		riderTripActiveKeyboard.ResizeKeyboard = true
		kbMsg := tgbotapi.NewMessage(chatID, "Ҳайдовчини харитада кузатинг ёки сафарни бекор қилишингиз мумкин.")
		kbMsg.ReplyMarkup = riderTripActiveKeyboard
		if _, err := s.riderBot.Send(kbMsg); err != nil {
			log.Printf("assignment_service: notify rider keyboard: %v", assignmentErrStr(err))
		}
	}

	// Remove the offer message from every notified driver (including the accepter). Native app accept
	// uses the same TryAssign as the bot; without deleting the accepter's row, the Telegram inline
	// "Accept" stayed visible even after the trip was taken in the app.
	notifRows, err := s.db.QueryContext(ctx, `
		SELECT chat_id, message_id FROM request_notifications
		WHERE request_id = ?1 AND message_id != 0`,
		requestID)
	if err != nil {
		return true, tripID, nil
	}
	defer notifRows.Close()
	for notifRows.Next() {
		var chatID int64
		var messageID int
		if err := notifRows.Scan(&chatID, &messageID); err != nil {
			continue
		}
		// Delete the order message instead of sending "So'rov allaqachon olindi".
		if s.driverBot != nil && messageID != 0 {
			del := tgbotapi.NewDeleteMessage(chatID, messageID)
			if _, err := s.driverBot.Request(del); err != nil {
				log.Printf("assignment_service: delete order message chat=%d msg=%d: %v", chatID, messageID, assignmentErrStr(err))
			}
		}
	}
	// Truncation here leaves other drivers with a live Accept button for a ride
	// that is already taken; surface it rather than reporting a clean assign.
	if err := notifRows.Err(); err != nil {
		log.Printf("assignment_service: clear offer messages for request %s: %v", requestID, err)
	}
	return true, tripID, nil
}

// RunExpiryWorker runs every 5 seconds: marks expired PENDING requests as EXPIRED and notifies each rider "Haydovchi topilmadi."
func (s *AssignmentService) RunExpiryWorker(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.expireRequests(ctx)
			s.expireAbandonedRequests(ctx)
			ExpireStaleDriverQueueOffers(ctx, s.db, s.cfg)
		}
	}
}

// abandonedRequestTimeout is how long a PENDING request with no confirmed
// destination may sit before it is retired. Generous, because a rider browsing
// the destination list is legitimately mid-flow.
const abandonedRequestTimeout = 30 * time.Minute

// expireAbandonedRequests retires PENDING requests that never reached a
// confirmed destination.
//
// expireRequests only touches requests that were actually dispatched (drop set
// and destination_confirmed = 1). A rider who sent their pickup and then closed
// Telegram left a PENDING row that nothing could ever clear — and because only
// one PENDING request per rider is allowed, that one abandoned row blocked every
// future order they tried to place, permanently, with the only escape being a
// /cancel command they had no reason to know about.
//
// These are expired silently: they were never broadcast, so there is no "no
// driver found" to report. The rider simply finds ordering works again.
func (s *AssignmentService) expireAbandonedRequests(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-abandonedRequestTimeout).Format("2006-01-02 15:04:05")
	res, err := s.db.ExecContext(ctx, `
		UPDATE ride_requests SET status = ?1
		WHERE status = ?2
		  AND COALESCE(destination_confirmed, 0) = 0
		  AND created_at <= ?3`,
		domain.RequestStatusExpired, domain.RequestStatusPending, cutoff)
	if err != nil {
		log.Printf("assignment_service: expire abandoned requests: %v", assignmentErrStr(err))
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("assignment_service: retired %d abandoned request(s) with no confirmed destination", n)
	}
}

func (s *AssignmentService) expireRequests(ctx context.Context) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("assignment_service: begin tx: %v", assignmentErrStr(err))
		return
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		UPDATE ride_requests SET status = ?1
		WHERE status = ?2 AND expires_at <= datetime('now')
		  AND drop_lat IS NOT NULL AND drop_lng IS NOT NULL
		  AND COALESCE(destination_confirmed, 0) = 1
		RETURNING id, rider_user_id`,
		domain.RequestStatusExpired, domain.RequestStatusPending)
	if err != nil {
		log.Printf("assignment_service: expire update: %v", assignmentErrStr(err))
		return
	}
	defer rows.Close()

	var riderUserIDs []int64
	var expiredRequestIDs []string
	for rows.Next() {
		var id string
		var riderUserID int64
		if err := rows.Scan(&id, &riderUserID); err != nil {
			continue
		}
		expiredRequestIDs = append(expiredRequestIDs, id)
		riderUserIDs = append(riderUserIDs, riderUserID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("assignment_service: commit: %v", assignmentErrStr(err))
		return
	}

	for _, requestID := range expiredRequestIDs {
		CloseAllDriverQueueOffers(ctx, s.db, requestID, "expired", false)
	}
	if len(expiredRequestIDs) > 0 {
		ws.NotifyDispatchChangedBurst()
	}

	for _, riderUserID := range riderUserIDs {
		if ShouldSkipRiderTripTelegramNotify(ctx, s.db, riderUserID) {
			continue
		}
		var telegramID int64
		err := s.db.QueryRowContext(ctx, `SELECT telegram_id FROM users WHERE id = ?1`, riderUserID).Scan(&telegramID)
		if err != nil || telegramID == 0 {
			continue
		}
		msg := tgbotapi.NewMessage(telegramID, "😔 Ҳозирча бўш ҳайдовчи топилмади.\n\nБироздан сўнг «🚕 Янги такси чақириш» орқали қайта уриниб кўринг.")
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Қайта қидириш", "search_again"),
			),
		)
		if _, err := s.riderBot.Send(msg); err != nil {
			log.Printf("assignment_service: notify rider expired: %v", assignmentErrStr(err))
		}
		// Restore main menu so rider has clear entry point
		mainMenu := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButton("🚕 Такси чақириш"),
				tgbotapi.NewKeyboardButton("ℹ️ Ёрдам"),
			),
		)
		mainMenu.ResizeKeyboard = true
		kbMsg := tgbotapi.NewMessage(telegramID, "Янги сўров учун «Такси чақириш» ни босинг.")
		kbMsg.ReplyMarkup = mainMenu
		if _, err := s.riderBot.Send(kbMsg); err != nil {
			log.Printf("assignment_service: rider main menu after expiry: %v", assignmentErrStr(err))
		}
	}
}

// RunRadiusExpansionWorker runs periodically: after RadiusExpansionMinutes, expands radius to ExpandedRadiusKm and re-broadcasts.
func (s *AssignmentService) RunRadiusExpansionWorker(ctx context.Context, matchSvc *MatchService) {
	if matchSvc == nil {
		return
	}
	// A request is widened once expansionDelay of its life has elapsed, which is
	// the same as saying it has at most (ttl - expansionDelay) left to run.
	ttl, expansionDelay := s.radiusExpansionWindow()
	remainingAtExpansion := ttl - expansionDelay
	if remainingAtExpansion < 0 {
		remainingAtExpansion = 0
	}
	// Must land inside a window measured in seconds, so tick faster than a minute.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.expandRadiusAndRebroadcast(ctx, matchSvc, remainingAtExpansion)
		}
	}
}

// radiusExpansionWindow returns the request TTL and how much of it elapses
// before the search radius widens.
//
// RADIUS_EXPANSION_MINUTES defaults to 5 minutes while REQUEST_EXPIRES_SECONDS
// defaults to 120, and the expansion query requires the request to still be
// unexpired — so with shipped defaults the widened search could never run at
// all, and riders in thin areas were told "no driver found" while drivers just
// outside the initial radius were never asked. Clamp the delay to a fraction of
// the actual TTL so expansion always gets a chance to fire.
func (s *AssignmentService) radiusExpansionWindow() (ttl, delay time.Duration) {
	ttl = 120 * time.Second
	if s.cfg != nil && s.cfg.RequestExpiresSeconds > 0 {
		ttl = time.Duration(s.cfg.RequestExpiresSeconds) * time.Second
	}
	maxDelay := ttl * 2 / 5 // widen once 40% of the window has elapsed

	delay = maxDelay
	if s.cfg != nil && s.cfg.RadiusExpansionMinutes > 0 {
		configured := time.Duration(s.cfg.RadiusExpansionMinutes) * time.Minute
		if configured <= maxDelay {
			delay = configured
		} else {
			log.Printf("assignment_service: RADIUS_EXPANSION_MINUTES=%d (%s) exceeds the usable window for a %s request TTL; widening after %s instead",
				s.cfg.RadiusExpansionMinutes, configured, ttl, maxDelay)
		}
	}
	if delay < time.Second {
		delay = time.Second
	}
	return ttl, delay
}

// expandRadiusAndRebroadcast widens requests that have at most
// remainingAtExpansion of their TTL left.
//
// Life remaining is measured from expires_at rather than created_at: the TTL is
// (re)set when the rider confirms, so created_at can be far older than the point
// at which dispatch actually started.
func (s *AssignmentService) expandRadiusAndRebroadcast(ctx context.Context, matchSvc *MatchService, remainingAtExpansion time.Duration) {
	remaining := fmt.Sprintf("+%d seconds", int(remainingAtExpansion.Seconds()))
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM ride_requests
		WHERE status = ?1 AND expires_at > datetime('now')
		  AND (radius_expanded_at IS NULL)
		  AND radius_km < ?2
		  AND expires_at <= datetime('now', ?3)
		  AND drop_lat IS NOT NULL AND drop_lng IS NOT NULL
		  AND COALESCE(destination_confirmed, 0) = 1`,
		domain.RequestStatusPending, s.cfg.ExpandedRadiusKm, remaining)
	if err != nil {
		log.Printf("assignment_service: radius expansion query: %v", assignmentErrStr(err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			continue
		}
		_, err := s.db.ExecContext(ctx, `
			UPDATE ride_requests SET radius_km = ?1, radius_expanded_at = datetime('now')
			WHERE id = ?2 AND status = ?3 AND radius_expanded_at IS NULL`,
			s.cfg.ExpandedRadiusKm, requestID, domain.RequestStatusPending)
		if err != nil {
			continue
		}
		log.Printf("assignment_service: expanded radius for request %s to %.1f km, re-broadcasting", requestID, s.cfg.ExpandedRadiusKm)
		if err := matchSvc.BroadcastRequest(ctx, requestID); err != nil {
			log.Printf("assignment_service: re-broadcast: %v", assignmentErrStr(err))
		}
	}
}

// riderMapWebAppKeyboard returns an inline keyboard with one Web App button (opens Telegram Mini App).
// Uses custom type because tgbotapi.InlineKeyboardButton does not expose web_app in this library version.
func riderMapWebAppKeyboard(buttonText, webAppURL string) riderMapInlineKeyboard {
	return riderMapInlineKeyboard{
		InlineKeyboard: [][]riderMapWebAppButton{{
			{Text: buttonText, WebApp: &riderMapWebAppInfo{URL: webAppURL}},
		}},
	}
}

type riderMapInlineKeyboard struct {
	InlineKeyboard [][]riderMapWebAppButton `json:"inline_keyboard"`
}
type riderMapWebAppButton struct {
	Text   string              `json:"text"`
	WebApp *riderMapWebAppInfo `json:"web_app,omitempty"`
}
type riderMapWebAppInfo struct {
	URL string `json:"url"`
}
