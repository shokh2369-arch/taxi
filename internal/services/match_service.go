package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/legal"
	"taxi-mvp/internal/utils"
)

const (
	acceptCallbackPrefix  = "accept:"
	defaultDriverCooldown = 5
	dispatchBatchSize     = 3  // send request to N nearest drivers per batch
	dispatchBatchWaitSec  = 60 // wait this many seconds for any driver in the batch to accept before trying next batch
	liveLocationOrderHint = "\n\n📍 Жонли локация ёқилган бўлса буюртмалар тезроқ келади."
	// Live location considered active only when last_live_location_at within 90s (same as dispatch).
	liveLocationActiveSeconds = 90
	// DriverLocationFreshnessSeconds: only drivers with last_seen_at within this many seconds are eligible for dispatch.
	driverLocationFreshnessSeconds = 90

	// To prevent "output too large" issues, never log large slices verbatim.
	logSliceMaxItems = 20
	logMaxChars      = 180
)

// Sentinel errors for admin manual offers (POST .../ride-requests/:id/offer).
var (
	ErrRideRequestNotOfferable   = errors.New("ride request not found or not pending")
	ErrAdminDriverNotEligible    = errors.New("driver not eligible for this offer")
	ErrAdminOfferExists          = errors.New("offer already recorded for this driver in an unexpected state")
	ErrAdminOfferAlreadyAccepted = errors.New("driver already accepted this request")
	ErrAdminOfferNoTelegram      = errors.New("driver bot unavailable for telegram delivery")
	ErrAdminOfferTelegramFail    = errors.New("failed to deliver offer via telegram")
)

func truncateLog(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	if maxChars < 4 {
		return s[:maxChars]
	}
	return s[:maxChars-3] + "..."
}

func isMissingColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column")
}

func isFreshWithin(last sql.NullString, seconds int) bool {
	if !last.Valid || strings.TrimSpace(last.String) == "" {
		return false
	}
	t, err := parseUTCTime(last.String)
	if err != nil {
		return false
	}
	return time.Since(t) <= time.Duration(seconds)*time.Second
}

func sampleInt64(ids []int64, maxItems int) string {
	if len(ids) == 0 {
		return "[]"
	}
	n := len(ids)
	if maxItems > 0 && n > maxItems {
		n = maxItems
	}
	base := fmt.Sprintf("%v", ids[:n])
	if maxItems > 0 && len(ids) > n {
		base += fmt.Sprintf("+%d_more", len(ids)-n)
	}
	return truncateLog(base, logMaxChars)
}

func sampleString(ss []string, maxItems int) string {
	if len(ss) == 0 {
		return "[]"
	}
	n := len(ss)
	if maxItems > 0 && n > maxItems {
		n = maxItems
	}
	base := "[" + strings.Join(ss[:n], ",") + "]"
	if maxItems > 0 && len(ss) > n {
		base += fmt.Sprintf("+%d_more", len(ss)-n)
	}
	return truncateLog(base, logMaxChars)
}

// formatOrderMessageToDriver builds the text sent to the driver for a new order (distance + client phone if available).
func formatOrderMessageToDriver(distKm float64, riderPhone string) string {
	text := fmt.Sprintf("Янги сўров (%.1f км узоқда).", distKm)
	if riderPhone != "" {
		text += "\n📞 Мижоз: " + riderPhone
	}
	text += "\nҚабул қиласизми?"
	return text
}

// parseUTCTime parses "2006-01-02 15:04:05" as UTC (stored timestamps are UTC).
func parseUTCTime(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
}

// isDriverSharingLiveLocation returns true only when last_live_location_at is within 90s (Telegram live updates only).
func (s *MatchService) isDriverSharingLiveLocation(ctx context.Context, driverUserID int64) bool {
	var lastLive sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT last_live_location_at FROM drivers WHERE user_id = ?1`, driverUserID).Scan(&lastLive); err != nil || !lastLive.Valid || lastLive.String == "" {
		return false
	}
	t, err := parseUTCTime(lastLive.String)
	if err != nil {
		return false
	}
	cutoff := time.Now().UTC().Add(-time.Duration(liveLocationActiveSeconds) * time.Second)
	return t.After(cutoff)
}

// MatchService handles ride request dispatch: batches of nearest drivers, 10s acceptance timeout per batch, then next batch.
type MatchService struct {
	db                *sql.DB
	bot               *tgbotapi.BotAPI
	cfg               *config.Config
	lastDriverNotif   map[int64]time.Time
	lastDriverNotifMu sync.Mutex
}

// NewMatchService returns a MatchService that sends request messages via the driver bot.
func NewMatchService(db *sql.DB, driverBot *tgbotapi.BotAPI, cfg *config.Config) *MatchService {
	return &MatchService{db: db, bot: driverBot, cfg: cfg, lastDriverNotif: make(map[int64]time.Time)}
}

// insertOfferNotification stores a SENT row for GET /driver/available-requests polling.
// chat_id/message_id 0 means no Telegram message (assignment_service skips delete when message_id is 0).
func (s *MatchService) insertOfferNotification(ctx context.Context, requestID string, driverUserID, chatID int64, messageID int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_notifications (request_id, driver_user_id, chat_id, message_id, status)
		VALUES (?1, ?2, ?3, ?4, ?5)`,
		requestID, driverUserID, chatID, messageID, domain.NotificationStatusSent)
	return err
}

// driverCandidate is an eligible driver with distance for sorting.
type driverCandidate struct {
	UserID     int64
	TelegramID int64
	LastLat    sql.NullFloat64
	LastLng    sql.NullFloat64
	AppLat     sql.NullFloat64
	AppLng     sql.NullFloat64
	AppLastAt  sql.NullString
	AppActive  int
	EffLat     float64
	EffLng     float64
	DistKm     float64
}

// StartPriorityDispatch starts a goroutine: notify closest driver, wait 8s, if no response notify next.
func (s *MatchService) StartPriorityDispatch(ctx context.Context, requestID string) {
	go s.runPriorityDispatch(ctx, requestID)
}

// requestStillDispatchable returns true if the request is PENDING and not past TTL (expires_at > now).
func (s *MatchService) requestStillDispatchable(ctx context.Context, requestID string) bool {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM ride_requests WHERE id = ?1 AND status = ?2 AND expires_at > datetime('now')`,
		requestID, domain.RequestStatusPending).Scan(&count)
	return err == nil && count == 1
}

func (s *MatchService) runPriorityDispatch(ctx context.Context, requestID string) {
	var pickupLat, pickupLng, radiusKm float64
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT pickup_lat, pickup_lng, radius_km, status FROM ride_requests WHERE id = ?1`,
		requestID).Scan(&pickupLat, &pickupLng, &radiusKm, &status)
	if err != nil || status != domain.RequestStatusPending {
		return
	}
	var riderPhone string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(u.phone,'') FROM ride_requests r JOIN users u ON u.id = r.rider_user_id WHERE r.id = ?1`, requestID).Scan(&riderPhone)
	riderPhone = strings.TrimSpace(riderPhone)
	// Request TTL: do not dispatch if already expired
	if !s.requestStillDispatchable(ctx, requestID) {
		log.Printf("dispatch_audit: request=%s skipped (expired or not PENDING)", requestID)
		return
	}
	// Grid prefilter: only look at drivers whose grid is in the 3x3 neighborhood of the pickup.
	gridIDs := utils.NeighborGridIDs(pickupLat, pickupLng)
	if s.cfg != nil && s.cfg.DispatchDebug {
		log.Printf("dispatch_debug: request=%s pickup=(%.5f,%.5f) grids_count=%d grids=%v", requestID, pickupLat, pickupLng, len(gridIDs), gridIDs)
	}
	// Online for dispatch when either:
	// - Telegram live is fresh (same behavior as before)
	// - Native app location is fresh (app_location_active=1 and app_last_seen_at within 90s)
	locationFreshSinceStr := time.Now().Add(-time.Duration(driverLocationFreshnessSeconds) * time.Second).UTC().Format("2006-01-02 15:04:05")
	// When InfiniteDriverBalance is true, all drivers get orders regardless of balance; otherwise require balance > 0.
	balanceCond := ""
	if s.cfg == nil || !s.cfg.InfiniteDriverBalance {
		balanceCond = " AND d.balance > 0"
	}
	placeholders := "?"
	// Use only anonymous '?' placeholders (avoid mixing ?1/?2 with '?', which can miscount in libSQL).
	args := []interface{}{locationFreshSinceStr, locationFreshSinceStr}
	for i := 1; i < len(gridIDs); i++ {
		placeholders += ",?"
	}
	for _, g := range gridIDs {
		args = append(args, g)
	}
	queryApp := `
		SELECT d.user_id, u.telegram_id, d.last_lat, d.last_lng,
		       d.app_lat, d.app_lng, d.app_last_seen_at, COALESCE(d.app_location_active, 0)
		FROM drivers d JOIN users u ON u.id = d.user_id
		WHERE 1=1` + balanceCond + `
		  AND COALESCE(d.is_active, 0) = 1
		  AND COALESCE(d.manual_offline, 0) = 0
		  AND (
				(COALESCE(d.live_location_active, 0) = 1
				 AND d.last_live_location_at IS NOT NULL AND d.last_live_location_at >= ?)
			 OR (COALESCE(d.app_location_active, 0) = 1
				 AND d.app_last_seen_at IS NOT NULL AND d.app_last_seen_at >= ?)
		  )` + `
		  AND d.verification_status = 'approved'
		  AND ` + legal.SQLDriverDispatchLegalOK + `
		  AND ((d.last_lat IS NOT NULL AND d.last_lng IS NOT NULL) OR (d.app_lat IS NOT NULL AND d.app_lng IS NOT NULL))
		  AND d.phone IS NOT NULL AND d.phone != ''
		  AND d.car_type IS NOT NULL AND d.car_type != ''
		  AND d.color IS NOT NULL AND d.color != ''
		  AND d.plate IS NOT NULL AND d.plate != ''
		  AND (d.grid_id IN (` + placeholders + `) OR d.grid_id IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM trips t WHERE t.driver_user_id = d.user_id AND t.status IN ('WAITING','ARRIVED','STARTED'))`

	queryTelegramOnly := `
		SELECT d.user_id, u.telegram_id, d.last_lat, d.last_lng
		FROM drivers d JOIN users u ON u.id = d.user_id
		WHERE 1=1` + balanceCond + `
		  AND COALESCE(d.is_active, 0) = 1
		  AND COALESCE(d.manual_offline, 0) = 0
		  AND COALESCE(d.live_location_active, 0) = 1
		  AND d.last_live_location_at IS NOT NULL AND d.last_live_location_at >= ?
		  AND d.verification_status = 'approved'
		  AND ` + legal.SQLDriverDispatchLegalOK + `
		  AND d.last_lat IS NOT NULL AND d.last_lng IS NOT NULL
		  AND d.phone IS NOT NULL AND d.phone != ''
		  AND d.car_type IS NOT NULL AND d.car_type != ''
		  AND d.color IS NOT NULL AND d.color != ''
		  AND d.plate IS NOT NULL AND d.plate != ''
		  AND (d.grid_id IN (` + placeholders + `) OR d.grid_id IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM trips t WHERE t.driver_user_id = d.user_id AND t.status IN ('WAITING','ARRIVED','STARTED'))`

	rows, err := s.db.QueryContext(ctx, queryApp, args...)
	appColsOK := true
	if err != nil && isMissingColumnErr(err) {
		appColsOK = false
		rows, err = s.db.QueryContext(ctx, queryTelegramOnly, args...)
	}
	if err != nil {
		log.Printf("match_service: dispatch query: %v", truncateLog(err.Error(), logMaxChars))
		return
	}
	defer rows.Close()
	var candidates []driverCandidate
	for rows.Next() {
		var uID int64
		var telegramID int64
		var lat, lng sql.NullFloat64
		var appLat, appLng sql.NullFloat64
		var appLast sql.NullString
		var appActive int
		if appColsOK {
			if err := rows.Scan(&uID, &telegramID, &lat, &lng, &appLat, &appLng, &appLast, &appActive); err != nil {
				continue
			}
		} else {
			if err := rows.Scan(&uID, &telegramID, &lat, &lng); err != nil {
				continue
			}
			appLat, appLng = sql.NullFloat64{}, sql.NullFloat64{}
			appLast = sql.NullString{}
			appActive = 0
		}
		loc := EffectiveDriverLocation{
			AppLat:            appLat,
			AppLng:            appLng,
			AppLastSeenAt:     appLast,
			AppLocationActive: sql.NullInt64{Int64: int64(appActive), Valid: appColsOK},
			LastLat:           lat,
			LastLng:           lng,
		}
		eLat, eLng := GetEffectiveDriverLocation(loc)
		log.Println("Driver location source:", GetEffectiveDriverLocationSource(loc))
		distKm := utils.HaversineMeters(pickupLat, pickupLng, eLat, eLng) / 1000
		if distKm > radiusKm {
			continue
		}
		candidates = append(candidates, driverCandidate{
			UserID:     uID,
			TelegramID: telegramID,
			LastLat:    lat,
			LastLng:    lng,
			AppLat:     appLat,
			AppLng:     appLng,
			AppLastAt:  appLast,
			AppActive:  appActive,
			EffLat:     eLat,
			EffLng:     eLng,
			DistKm:     distKm,
		})
	}
	// Audit: candidate drivers (always log for dispatch audit)
	{
		ids := make([]int64, 0, len(candidates))
		for _, c := range candidates {
			ids = append(ids, c.UserID)
		}
		log.Printf("dispatch_audit: request=%s candidate_drivers=%d driver_ids_sample=%s", requestID, len(candidates), sampleInt64(ids, logSliceMaxItems))
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: request=%s candidate_drivers=%d ids_sample=%s", requestID, len(candidates), sampleInt64(ids, logSliceMaxItems))
		}
	}
	if len(candidates) == 0 {
		log.Printf("match_service: no eligible drivers for request %s (live location + balance%s, within radius)", requestID, balanceCond)
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].DistKm < candidates[j].DistKm })
	cooldownSec := defaultDriverCooldown
	if s.cfg != nil && s.cfg.DispatchDriverCooldownSec > 0 {
		cooldownSec = s.cfg.DispatchDriverCooldownSec
	}
	batchWaitSec := dispatchBatchWaitSec
	if s.cfg != nil && s.cfg.DispatchWaitSeconds > 0 {
		batchWaitSec = s.cfg.DispatchWaitSeconds
	}

	// Process drivers in batches of N; wait 10s per batch for acceptance; if no accept, send to next batch.
	for batchStart := 0; batchStart < len(candidates); batchStart += dispatchBatchSize {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !s.requestStillDispatchable(ctx, requestID) {
			log.Printf("dispatch_audit: request=%s stopped (expired or no longer PENDING)", requestID)
			return
		}
		batchEnd := batchStart + dispatchBatchSize
		if batchEnd > len(candidates) {
			batchEnd = len(candidates)
		}
		batch := candidates[batchStart:batchEnd]
		var batchDriverIDs []int64

		for _, c := range batch {
			var currentStatus string
			if err := s.db.QueryRowContext(ctx, `SELECT status FROM ride_requests WHERE id = ?1`, requestID).Scan(&currentStatus); err != nil || currentStatus != domain.RequestStatusPending {
				return
			}
			var exists int
			if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM request_notifications WHERE request_id = ?1 AND driver_user_id = ?2`, requestID, c.UserID).Scan(&exists); err == nil {
				continue
			}
			s.lastDriverNotifMu.Lock()
			last, ok := s.lastDriverNotif[c.UserID]
			if ok && time.Since(last) < time.Duration(cooldownSec)*time.Second {
				s.lastDriverNotifMu.Unlock()
				continue
			}
			s.lastDriverNotif[c.UserID] = time.Now()
			s.lastDriverNotifMu.Unlock()
			log.Printf("dispatch_audit: request=%s batch_send driver=%d dist_km=%.3f", requestID, c.UserID, c.DistKm)
			if s.cfg != nil && s.cfg.DispatchDebug {
				log.Printf("dispatch_debug: request=%s try_driver=%d dist_km=%.3f", requestID, c.UserID, c.DistKm)
			}
			text := formatOrderMessageToDriver(c.DistKm, riderPhone)
			if !s.isDriverSharingLiveLocation(ctx, c.UserID) {
				text += liveLocationOrderHint
			}
			// Delivery choice:
			// - If driver is online via native app (fresh app_last_seen_at), do NOT send Telegram — app polls request_notifications.
			// - Otherwise, send Telegram as before.
			appFresh := c.AppActive == 1 && isFreshWithin(c.AppLastAt, driverLocationFreshnessSeconds)
			chatID, msgID := int64(0), 0
			if !appFresh {
				msg := tgbotapi.NewMessage(c.TelegramID, text)
				msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("✅ Қабул қилиш", acceptCallbackPrefix+requestID),
					),
				)
				sentMsg, sendErr := s.bot.Send(msg)
				chatID = c.TelegramID
				if sendErr != nil {
					log.Printf("match_service: send to driver %d: %v", c.TelegramID, truncateLog(sendErr.Error(), logMaxChars))
					// For Telegram delivery failures, only keep the app polling row when HTTP live is enabled.
					// For app-fresh drivers we already skip Telegram entirely.
					if s.cfg == nil || !s.cfg.EnableDriverHTTPLiveLocation {
						continue
					}
					chatID = 0
					msgID = 0
				} else {
					msgID = sentMsg.MessageID
				}
			}
			if err := s.insertOfferNotification(ctx, requestID, c.UserID, chatID, msgID); err != nil {
				log.Printf("match_service: insert request_notifications request=%s driver=%d: %v", requestID, c.UserID, truncateLog(err.Error(), logMaxChars))
				continue
			}
			batchDriverIDs = append(batchDriverIDs, c.UserID)
		}

		// Wait batchWaitSec for any driver in this batch to accept; poll every second.
		for wait := 0; wait < batchWaitSec; wait++ {
			time.Sleep(1 * time.Second)
			var st string
			if err := s.db.QueryRowContext(ctx, `SELECT status FROM ride_requests WHERE id = ?1`, requestID).Scan(&st); err != nil {
				return
			}
			if st != domain.RequestStatusPending {
				return // accepted or cancelled/expired
			}
		}

		// Nobody in this batch accepted; mark their notifications as timeout and continue to next batch.
		for _, driverID := range batchDriverIDs {
			_, _ = s.db.ExecContext(ctx, `UPDATE request_notifications SET status = ?1 WHERE request_id = ?2 AND driver_user_id = ?3`,
				domain.NotificationStatusTimeout, requestID, driverID)
		}
		log.Printf("dispatch_audit: request=%s batch_timeout drivers_count=%d drivers_sample=%s after=%ds next_batch", requestID, len(batchDriverIDs), sampleInt64(batchDriverIDs, logSliceMaxItems), batchWaitSec)
	}
}

// BroadcastRequest starts batched priority dispatch (nearest first, 10s per batch, then next batch). Used by rider and radius expansion.
func (s *MatchService) BroadcastRequest(ctx context.Context, requestID string) error {
	s.StartPriorityDispatch(ctx, requestID)
	return nil
}

// PulseDriverOnlineFromHTTP marks an eligible driver online and triggers NotifyDriverOfPendingRequests after POST /driver/location
// when cfg.EnableDriverHTTPLiveLocation is true (default). Same DB gates as dispatch (approved, legal, balance, profile fields).
// No-op if the driver already has an active trip or fails eligibility. Telegram bot online/live paths are separate and unchanged.
func (s *MatchService) PulseDriverOnlineFromHTTP(ctx context.Context, driverUserID int64) {
	if s == nil || s.db == nil {
		return
	}
	var activeTrip string
	_ = s.db.QueryRowContext(ctx, `
		SELECT id FROM trips WHERE driver_user_id = ?1 AND status IN ('WAITING','ARRIVED','STARTED') LIMIT 1`,
		driverUserID).Scan(&activeTrip)
	if activeTrip != "" {
		return
	}
	balanceCond := " AND d.balance > 0"
	if s.cfg != nil && s.cfg.InfiniteDriverBalance {
		balanceCond = ""
	}
	var uid int64
	err := s.db.QueryRowContext(ctx, `
		SELECT d.user_id FROM drivers d
		WHERE d.user_id = ?1 AND d.verification_status = 'approved' AND `+legal.SQLDriverDispatchLegalOK+`
		AND d.phone IS NOT NULL AND d.phone != ''
		AND d.car_type IS NOT NULL AND d.car_type != ''
		AND d.color IS NOT NULL AND d.color != ''
		AND d.plate IS NOT NULL AND d.plate != ''`+balanceCond,
		driverUserID).Scan(&uid)
	if err != nil {
		return
	}
	nowStr := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, _ = s.db.ExecContext(ctx, `UPDATE drivers SET is_active = 1, manual_offline = 0, last_seen_at = ?1 WHERE user_id = ?2`, nowStr, driverUserID)
	go s.NotifyDriverOfPendingRequests(context.Background(), driverUserID)
}

// NotifyDriverOfPendingRequests sends any PENDING ride requests (within this driver's radius) to a driver who just came online.
// Skips requests already sent to this driver (request_notifications). Does not change existing dispatch logic.
// A short delay allows a nearly-simultaneous rider request to be committed so the driver receives it.
// Only sends if the driver's location is fresh (last_seen_at within 90s).
func (s *MatchService) NotifyDriverOfPendingRequests(ctx context.Context, driverUserID int64) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(800 * time.Millisecond):
	}
	var telegramID int64
	var lat, lng sql.NullFloat64
	var appLat, appLng sql.NullFloat64
	var appLast sql.NullString
	var appActive int
	var isActive int
	var lastSeenAt, lastLiveAt sql.NullString
	var liveLocationActive int
	balanceCond := " AND d.balance > 0"
	if s.cfg != nil && s.cfg.InfiniteDriverBalance {
		balanceCond = ""
	}
	qApp := `
		SELECT u.telegram_id, d.last_lat, d.last_lng,
		       d.app_lat, d.app_lng, d.app_last_seen_at, COALESCE(d.app_location_active, 0),
		       d.is_active, COALESCE(d.manual_offline, 0), d.last_seen_at, d.last_live_location_at, COALESCE(d.live_location_active, 0)
		FROM drivers d JOIN users u ON u.id = d.user_id
		WHERE d.user_id = ?1 AND d.verification_status = 'approved' AND ` + legal.SQLDriverDispatchLegalOK + `
		  AND ((d.last_lat IS NOT NULL AND d.last_lng IS NOT NULL) OR (d.app_lat IS NOT NULL AND d.app_lng IS NOT NULL))` + balanceCond
	qTelegramOnly := `
		SELECT u.telegram_id, d.last_lat, d.last_lng, d.is_active, COALESCE(d.manual_offline, 0), d.last_seen_at, d.last_live_location_at, COALESCE(d.live_location_active, 0)
		FROM drivers d JOIN users u ON u.id = d.user_id
		WHERE d.user_id = ?1 AND d.verification_status = 'approved' AND ` + legal.SQLDriverDispatchLegalOK + `
		  AND d.last_lat IS NOT NULL AND d.last_lng IS NOT NULL` + balanceCond

	appColsOK := true
	var manualOffline int
	err := s.db.QueryRowContext(ctx, qApp, driverUserID).Scan(&telegramID, &lat, &lng, &appLat, &appLng, &appLast, &appActive, &isActive, &manualOffline, &lastSeenAt, &lastLiveAt, &liveLocationActive)
	if err != nil && isMissingColumnErr(err) {
		appColsOK = false
		err = s.db.QueryRowContext(ctx, qTelegramOnly, driverUserID).Scan(&telegramID, &lat, &lng, &isActive, &manualOffline, &lastSeenAt, &lastLiveAt, &liveLocationActive)
		appLat, appLng = sql.NullFloat64{}, sql.NullFloat64{}
		appLast = sql.NullString{}
		appActive = 0
	}
	if err != nil {
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: driver=%d skipped: not_eligible_or_missing err=%v", driverUserID, truncateLog(err.Error(), logMaxChars))
		}
		return
	}
	loc := EffectiveDriverLocation{
		AppLat:            appLat,
		AppLng:            appLng,
		AppLastSeenAt:     appLast,
		AppLocationActive: sql.NullInt64{Int64: int64(appActive), Valid: appColsOK},
		LastLat:           lat,
		LastLng:           lng,
	}
	eLat, eLng := GetEffectiveDriverLocation(loc)
	log.Println("Driver location source:", GetEffectiveDriverLocationSource(loc))
	if eLat == 0 && eLng == 0 {
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: driver=%d skipped: no_effective_location", driverUserID)
		}
		return
	}
	if isActive != 1 {
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: driver=%d skipped: is_active=%d", driverUserID, isActive)
		}
		return
	}
	if manualOffline == 1 {
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: driver=%d skipped: manual_offline=1", driverUserID)
		}
		return
	}
	// Online / dispatch eligible when either app location is fresh OR Telegram live is fresh.
	appFresh := false
	if appActive == 1 && appLast.Valid && appLast.String != "" {
		if t, err := parseUTCTime(appLast.String); err == nil {
			appFresh = time.Since(t) <= driverLocationFreshnessSeconds*time.Second
		}
	}
	telegramFresh := false
	if liveLocationActive == 1 && lastLiveAt.Valid && lastLiveAt.String != "" {
		if t, err := parseUTCTime(lastLiveAt.String); err == nil {
			telegramFresh = time.Since(t) <= driverLocationFreshnessSeconds*time.Second
		}
	}
	if !appFresh && !telegramFresh {
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: driver=%d skipped: stale_location app_fresh=%v telegram_fresh=%v", driverUserID, appFresh, telegramFresh)
		}
		return
	}
	// Skip if driver already has an active (WAITING/ARRIVED/STARTED) trip.
	var activeTripID string
	_ = s.db.QueryRowContext(ctx, `
		SELECT id FROM trips WHERE driver_user_id = ?1 AND status IN ('WAITING','ARRIVED','STARTED') LIMIT 1`,
		driverUserID).Scan(&activeTripID)
	if activeTripID != "" {
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: driver=%d skipped: has_active_trip=%s", driverUserID, activeTripID)
		}
		return
	}
	// Limit to recent waiting requests to avoid heavy scans.
	windowSec := s.cfg.RequestExpiresSeconds
	if windowSec <= 0 || windowSec > 3600 {
		windowSec = 600
	}
	cutoff := time.Now().Add(-time.Duration(windowSec) * time.Second).UTC().Format("2006-01-02 15:04:05")
	args := []interface{}{domain.RequestStatusPending, cutoff}
	args = append(args, driverUserID)
	// Only send requests that are still within TTL (expires_at > now)
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.pickup_lat, r.pickup_lng, r.radius_km, r.pickup_grid
		FROM ride_requests r
		WHERE r.status = ?
		  AND r.created_at >= ?
		  AND r.expires_at > datetime('now')
		  AND NOT EXISTS (SELECT 1 FROM request_notifications n WHERE n.request_id = r.id AND n.driver_user_id = ?)`,
		args...)
	if err != nil {
		log.Printf("match_service: NotifyDriverOfPendingRequests query: %v", truncateLog(err.Error(), logMaxChars))
		return
	}
	defer rows.Close()
	var toSend []struct {
		requestID string
		distKm    float64
	}
	for rows.Next() {
		var requestID string
		var pickupLat, pickupLng, radiusKm float64
		var pickupGrid sql.NullString
		if err := rows.Scan(&requestID, &pickupLat, &pickupLng, &radiusKm, &pickupGrid); err != nil {
			continue
		}
		distKm := utils.HaversineMeters(pickupLat, pickupLng, eLat, eLng) / 1000
		if distKm > radiusKm {
			if s.cfg != nil && s.cfg.DispatchDebug {
				log.Printf("dispatch_debug: driver=%d request=%s pickup_grid=%s dist_km=%.3f reason=outside_radius",
					driverUserID, requestID, pickupGrid.String, distKm)
			}
			continue
		}
		toSend = append(toSend, struct {
			requestID string
			distKm    float64
		}{requestID, distKm})
	}
	if s.cfg != nil && s.cfg.DispatchDebug {
		reqIDs := make([]string, 0, len(toSend))
		for _, it := range toSend {
			reqIDs = append(reqIDs, it.requestID)
		}
		log.Printf("dispatch_debug: driver=%d candidate_requests=%d req_ids_sample=%s", driverUserID, len(toSend), sampleString(reqIDs, logSliceMaxItems))
	}
	if err := rows.Err(); err != nil {
		return
	}
	for _, item := range toSend {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var status string
		if err := s.db.QueryRowContext(ctx, `SELECT status FROM ride_requests WHERE id = ?1`, item.requestID).Scan(&status); err != nil || status != domain.RequestStatusPending {
			if s.cfg != nil && s.cfg.DispatchDebug {
				log.Printf("dispatch_debug: driver=%d request=%s skipped status=%s", driverUserID, item.requestID, status)
			}
			continue
		}
		if s.cfg != nil && s.cfg.DispatchDebug {
			log.Printf("dispatch_debug: driver=%d notify_request=%s dist_km=%.3f", driverUserID, item.requestID, item.distKm)
		}
		cooldownSec := defaultDriverCooldown
		if s.cfg != nil && s.cfg.DispatchDriverCooldownSec > 0 {
			cooldownSec = s.cfg.DispatchDriverCooldownSec
		}
		s.lastDriverNotifMu.Lock()
		last, ok := s.lastDriverNotif[driverUserID]
		if ok && time.Since(last) < time.Duration(cooldownSec)*time.Second {
			s.lastDriverNotifMu.Unlock()
			continue
		}
		s.lastDriverNotif[driverUserID] = time.Now()
		s.lastDriverNotifMu.Unlock()
		var riderPhone string
		_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(u.phone,'') FROM ride_requests r JOIN users u ON u.id = r.rider_user_id WHERE r.id = ?1`, item.requestID).Scan(&riderPhone)
		riderPhone = strings.TrimSpace(riderPhone)
		text := formatOrderMessageToDriver(item.distKm, riderPhone)
		if !s.isDriverSharingLiveLocation(ctx, driverUserID) {
			text += liveLocationOrderHint
		}
		msg := tgbotapi.NewMessage(telegramID, text)
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Қабул қилиш", acceptCallbackPrefix+item.requestID),
			),
		)
		chatID, msgID := int64(0), 0
		if !appFresh {
			sentMsg, sendErr := s.bot.Send(msg)
			chatID = telegramID
			if sendErr != nil {
				log.Printf("match_service: send pending request to driver %d: %v", driverUserID, truncateLog(sendErr.Error(), logMaxChars))
				if s.cfg == nil || !s.cfg.EnableDriverHTTPLiveLocation {
					continue
				}
				chatID = 0
				msgID = 0
			} else {
				msgID = sentMsg.MessageID
			}
		}
		if err := s.insertOfferNotification(ctx, item.requestID, driverUserID, chatID, msgID); err != nil {
			log.Printf("match_service: insert pending notification request=%s driver=%d: %v", item.requestID, driverUserID, truncateLog(err.Error(), logMaxChars))
			continue
		}
		time.Sleep(1 * time.Second)
	}
}

// AdminNearestDispatchDriver is one driver who may receive a manual admin offer, with distance from pickup.
// Distance is computed for sorting/display only.
type AdminNearestDispatchDriver struct {
	ID         int64   `json:"id"`
	TelegramID int64   `json:"telegram_id"`
	DistanceKm float64 `json:"distance_km"`
	LastLat    float64 `json:"last_lat"`
	LastLng    float64 `json:"last_lng"`
}

// sqlAdminOfferDriverPredicates is the shared WHERE clause (after JOIN) for admin manual offers.
// This must stay aligned with what the admin UI can select: map + GET /nearest-requests do not require
// balance, legal acceptances, or fresh live location. Automatic dispatch still enforces those separately.
func (s *MatchService) sqlAdminOfferDriverPredicates() string {
	return `d.verification_status = 'approved'
		  AND ((d.last_lat IS NOT NULL AND d.last_lng IS NOT NULL) OR (d.app_lat IS NOT NULL AND d.app_lng IS NOT NULL))
		  AND NOT EXISTS (SELECT 1 FROM trips t WHERE t.driver_user_id = d.user_id AND t.status IN ('WAITING','ARRIVED','STARTED'))`
}

// AdminNearestDispatchDrivers returns drivers the admin may manually offer: approved, last known coordinates,
// and not on an active trip. **Distance does not filter results**; results are sorted by km from pickup.
func (s *MatchService) AdminNearestDispatchDrivers(ctx context.Context, requestID string) ([]AdminNearestDispatchDriver, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("match service unavailable")
	}
	var pickupLat, pickupLng float64
	if err := s.db.QueryRowContext(ctx, `SELECT pickup_lat, pickup_lng FROM ride_requests WHERE id = ?1`, requestID).Scan(&pickupLat, &pickupLng); err != nil {
		return nil, err
	}

	pred := s.sqlAdminOfferDriverPredicates()
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.user_id, u.telegram_id, d.last_lat, d.last_lng,
		       d.app_lat, d.app_lng, d.app_last_seen_at, COALESCE(d.app_location_active, 0)
		FROM drivers d JOIN users u ON u.id = d.user_id
		WHERE `+pred)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []driverCandidate
	for rows.Next() {
		var uID, telegramID int64
		var lat, lng sql.NullFloat64
		var appLat, appLng sql.NullFloat64
		var appLast sql.NullString
		var appActive int
		if err := rows.Scan(&uID, &telegramID, &lat, &lng, &appLat, &appLng, &appLast, &appActive); err != nil {
			continue
		}
		loc := EffectiveDriverLocation{
			AppLat:            appLat,
			AppLng:            appLng,
			AppLastSeenAt:     appLast,
			AppLocationActive: sql.NullInt64{Int64: int64(appActive), Valid: true},
			LastLat:           lat,
			LastLng:           lng,
		}
		eLat, eLng := GetEffectiveDriverLocation(loc)
		log.Println("Driver location source:", GetEffectiveDriverLocationSource(loc))
		distKm := utils.HaversineMeters(pickupLat, pickupLng, eLat, eLng) / 1000
		candidates = append(candidates, driverCandidate{
			UserID:     uID,
			TelegramID: telegramID,
			LastLat:    lat,
			LastLng:    lng,
			AppLat:     appLat,
			AppLng:     appLng,
			AppLastAt:  appLast,
			AppActive:  appActive,
			EffLat:     eLat,
			EffLng:     eLng,
			DistKm:     distKm,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].DistKm < candidates[j].DistKm })
	out := make([]AdminNearestDispatchDriver, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, AdminNearestDispatchDriver{
			ID:         c.UserID,
			TelegramID: c.TelegramID,
			DistanceKm: c.DistKm,
			LastLat:    c.EffLat,
			LastLng:    c.EffLng,
		})
	}
	return out, nil
}

// AdminSendOfferToDriver sends the same Telegram + request_notifications flow as automatic dispatch,
// for one driver chosen from the admin map. Eligibility matches AdminNearestDispatchDrivers.
func (s *MatchService) AdminSendOfferToDriver(ctx context.Context, requestID string, driverUserID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("match service unavailable")
	}
	if driverUserID <= 0 {
		return ErrAdminDriverNotEligible
	}

	var pickupLat, pickupLng float64
	err := s.db.QueryRowContext(ctx, `
		SELECT pickup_lat, pickup_lng FROM ride_requests
		WHERE id = ?1 AND status = ?2 AND expires_at > datetime('now')
		  AND (assigned_driver_user_id IS NULL OR assigned_driver_user_id = 0)`,
		requestID, domain.RequestStatusPending).Scan(&pickupLat, &pickupLng)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRideRequestNotOfferable
		}
		return err
	}

	var existingNotifStatus string
	var hasNotif bool
	err = s.db.QueryRowContext(ctx, `
		SELECT status FROM request_notifications WHERE request_id = ?1 AND driver_user_id = ?2`,
		requestID, driverUserID).Scan(&existingNotifStatus)
	if err == nil {
		hasNotif = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if hasNotif {
		switch existingNotifStatus {
		case domain.NotificationStatusAccepted:
			return ErrAdminOfferAlreadyAccepted
		case domain.NotificationStatusSent, domain.NotificationStatusTimeout, domain.NotificationStatusRejected:
			// Admin resend: same row, new Telegram message + refresh polling row.
		default:
			return fmt.Errorf("%w (status=%q)", ErrAdminOfferExists, existingNotifStatus)
		}
	}

	pred := s.sqlAdminOfferDriverPredicates()
	var telegramID int64
	var lastLat, lastLng sql.NullFloat64
	var appLat, appLng sql.NullFloat64
	var appLast sql.NullString
	var appActive int
	err = s.db.QueryRowContext(ctx, `
		SELECT u.telegram_id, d.last_lat, d.last_lng,
		       d.app_lat, d.app_lng, d.app_last_seen_at, COALESCE(d.app_location_active, 0)
		FROM drivers d JOIN users u ON u.id = d.user_id
		WHERE d.user_id = ?1 AND `+pred,
		driverUserID).Scan(&telegramID, &lastLat, &lastLng, &appLat, &appLng, &appLast, &appActive)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAdminDriverNotEligible
		}
		return err
	}

	loc := EffectiveDriverLocation{
		AppLat:            appLat,
		AppLng:            appLng,
		AppLastSeenAt:     appLast,
		AppLocationActive: sql.NullInt64{Int64: int64(appActive), Valid: true},
		LastLat:           lastLat,
		LastLng:           lastLng,
	}
	eLat, eLng := GetEffectiveDriverLocation(loc)
	log.Println("Driver location source:", GetEffectiveDriverLocationSource(loc))
	distKm := utils.HaversineMeters(pickupLat, pickupLng, eLat, eLng) / 1000
	var riderPhone string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(u.phone,'') FROM ride_requests r JOIN users u ON u.id = r.rider_user_id WHERE r.id = ?1`, requestID).Scan(&riderPhone)
	riderPhone = strings.TrimSpace(riderPhone)

	text := formatOrderMessageToDriver(distKm, riderPhone)
	if !s.isDriverSharingLiveLocation(ctx, driverUserID) {
		text += liveLocationOrderHint
	}

	// If the driver is online via app (fresh app_last_seen_at), prefer app polling delivery (no Telegram send).
	appFresh := appActive == 1 && isFreshWithin(appLast, driverLocationFreshnessSeconds)
	chatID, msgID := int64(0), 0
	if !appFresh {
		chatID = telegramID
		if s.bot != nil {
			msg := tgbotapi.NewMessage(telegramID, text)
			msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("✅ Қабул қилиш", acceptCallbackPrefix+requestID),
				),
			)
			sentMsg, sendErr := s.bot.Send(msg)
			if sendErr != nil {
				log.Printf("admin_offer: telegram send driver=%d request=%s err=%v", driverUserID, requestID, truncateLog(sendErr.Error(), logMaxChars))
				if s.cfg == nil || !s.cfg.EnableDriverHTTPLiveLocation {
					return fmt.Errorf("%w: %v", ErrAdminOfferTelegramFail, sendErr)
				}
				chatID = 0
				msgID = 0
			} else {
				msgID = sentMsg.MessageID
			}
		} else {
			if s.cfg == nil || !s.cfg.EnableDriverHTTPLiveLocation {
				return ErrAdminOfferNoTelegram
			}
			chatID = 0
			msgID = 0
		}
	}

	if !hasNotif {
		if err := s.insertOfferNotification(ctx, requestID, driverUserID, chatID, msgID); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrAdminOfferExists
			}
			return err
		}
		return nil
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE request_notifications
		SET chat_id = ?1, message_id = ?2, status = ?3
		WHERE request_id = ?4 AND driver_user_id = ?5`,
		chatID, msgID, domain.NotificationStatusSent, requestID, driverUserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAdminOfferExists
	}
	return nil
}
