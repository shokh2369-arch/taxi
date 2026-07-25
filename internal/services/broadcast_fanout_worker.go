package services

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/config"
)

const (
	// One send per tick keeps delivery under Telegram's rate limit.
	broadcastSendInterval = 120 * time.Millisecond
	// How many candidates one query fetches, then drained one per tick.
	broadcastBatchSize = 50
	// Idle polling backoff for the candidate query.
	broadcastMinPollDelay = 1 * time.Second
	broadcastMaxPollDelay = 60 * time.Second
	// How often elapsed per-chat cooldowns are swept out of the map.
	broadcastCooldownEvictEvery = 5 * time.Minute
)

// RunBroadcastFanoutWorker delivers published broadcasts to all rider Telegram users (users.role='rider'),
// idempotent via broadcast_telegram_deliveries.
func RunBroadcastFanoutWorker(ctx context.Context, db *sql.DB, riderBot *tgbotapi.BotAPI, _ *config.Config) {
	if db == nil || riderBot == nil {
		return
	}
	// Conservative base rate: ~10 msg/s. On 429, pause according to retry_after.
	tick := time.NewTicker(broadcastSendInterval)
	defer tick.Stop()

	var globalNext time.Time
	perChatNext := make(map[int64]time.Time)

	// The candidate query is a broadcast_posts x users anti-join. Running it on
	// every 120ms tick cost ~720k scans/day with nothing published, and made
	// delivering to N riders re-run it N times. Instead: fetch a batch, drain it
	// one send per tick, and when there is nothing to send back off the polling
	// interval so an idle system is nearly silent.
	var queue []broadcastCandidate
	var nextPoll time.Time
	pollDelay := broadcastMinPollDelay
	lastEvict := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now := time.Now()
			if globalNext.After(now) {
				continue
			}

			// Bounded memory: drop per-chat cooldowns that have already elapsed.
			if now.Sub(lastEvict) >= broadcastCooldownEvictEvery {
				for chatID, until := range perChatNext {
					if !until.After(now) {
						delete(perChatNext, chatID)
					}
				}
				lastEvict = now
			}

			if len(queue) == 0 {
				if now.Before(nextPoll) {
					continue
				}
				cands, err := pickBroadcastCandidates(ctx, db, broadcastBatchSize)
				if err != nil {
					if err != sql.ErrNoRows {
						log.Printf("broadcast fanout: pick: %v", err)
					}
					nextPoll = now.Add(pollDelay)
					continue
				}
				if len(cands) == 0 {
					// Nothing pending: back off so an idle worker stops hammering the DB.
					if pollDelay *= 2; pollDelay > broadcastMaxPollDelay {
						pollDelay = broadcastMaxPollDelay
					}
					nextPoll = now.Add(pollDelay)
					continue
				}
				pollDelay = broadcastMinPollDelay
				queue = cands
			}

			var chosen *broadcastCandidate
			for i := range queue {
				ch := queue[i].ChatID
				if ch == 0 {
					continue
				}
				if next, ok := perChatNext[ch]; ok && next.After(now) {
					continue
				}
				if strings.TrimSpace(queue[i].BroadcastID) == "" {
					continue
				}
				hasBody := strings.TrimSpace(queue[i].Body) != ""
				hasMedia := queue[i].MediaURL.Valid && strings.TrimSpace(queue[i].MediaURL.String) != ""
				if !hasBody && !hasMedia {
					continue
				}
				chosen = &queue[i]
				queue = append(queue[:i], queue[i+1:]...)
				break
			}
			if chosen == nil {
				// Every queued candidate is unusable or cooling down; refetch later.
				queue = nil
				nextPoll = now.Add(broadcastMinPollDelay)
				continue
			}

			if err := sendBroadcastCandidate(riderBot, *chosen); err != nil {
				if retryAfter := telegramRetryAfterSeconds(err); retryAfter > 0 {
					wait := time.Duration(retryAfter)*time.Second + 250*time.Millisecond
					next := time.Now().Add(wait)
					globalNext = next
					perChatNext[chosen.ChatID] = next
					log.Printf("broadcast fanout: 429 retry_after=%ds broadcast_id=%s chat_id=%d", retryAfter, chosen.BroadcastID, chosen.ChatID)
					continue
				}
				if isTelegramDeliveryPermanentErr(err) {
					markBroadcastDeliverySkipped(ctx, db, chosen.BroadcastID, chosen.ChatID)
					markUserTelegramBlocked(ctx, db, chosen.ChatID)
					log.Printf("broadcast fanout: skip unreachable chat_id=%d broadcast_id=%s: %v", chosen.ChatID, chosen.BroadcastID, err)
					continue
				}
				// Transient errors: backoff this chat briefly and retry later.
				perChatNext[chosen.ChatID] = time.Now().Add(10 * time.Second)
				log.Printf("broadcast fanout: send broadcast_id=%s chat_id=%d: %v", chosen.BroadcastID, chosen.ChatID, err)
				continue
			}

			// Mark delivered (idempotent primary key).
			_, _ = db.ExecContext(ctx, `
				INSERT OR IGNORE INTO broadcast_telegram_deliveries (broadcast_id, chat_id, delivered_at)
				VALUES (?1, ?2, datetime('now'))`, chosen.BroadcastID, chosen.ChatID)
		}
	}
}

type broadcastCandidate struct {
	BroadcastID string
	ChatID      int64
	Title       sql.NullString
	Body        string
	MediaURL    sql.NullString
	MediaType   sql.NullString
}

func pickBroadcastCandidates(ctx context.Context, db *sql.DB, limit int) ([]broadcastCandidate, error) {
	if db == nil {
		return nil, sql.ErrConnDone
	}
	if limit <= 0 {
		limit = 1
	}
	rows, err := db.QueryContext(ctx, `
		WITH candidates AS (
			SELECT b.id AS broadcast_id, u.telegram_id AS chat_id
			FROM broadcast_posts b
			JOIN users u ON u.role = 'rider' AND u.telegram_id != 0
			       AND COALESCE(u.telegram_bot_blocked, 0) = 0
			LEFT JOIN broadcast_telegram_deliveries d
			       ON d.broadcast_id = b.id AND d.chat_id = u.telegram_id
			WHERE b.status = 'published'
			  AND (
			    COALESCE(TRIM(b.body), '') != ''
			    OR COALESCE(TRIM(b.cloudinary_secure_url), '') != ''
			  )
			  AND COALESCE(b.audience, 'all_riders') = 'all_riders'
			  AND d.broadcast_id IS NULL
			ORDER BY datetime(b.created_at) DESC, b.id DESC, u.id ASC
			LIMIT ?1
		)
		SELECT c.broadcast_id,
		       c.chat_id,
		       b.title,
		       b.body,
		       b.cloudinary_secure_url,
		       b.media_type
		FROM candidates c
		JOIN broadcast_posts b ON b.id = c.broadcast_id
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []broadcastCandidate
	for rows.Next() {
		var c broadcastCandidate
		if err := rows.Scan(&c.BroadcastID, &c.ChatID, &c.Title, &c.Body, &c.MediaURL, &c.MediaType); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func sendBroadcastCandidate(bot *tgbotapi.BotAPI, c broadcastCandidate) error {
	if bot == nil || c.ChatID == 0 {
		return nil
	}
	body := strings.TrimSpace(c.Body)
	if strings.TrimSpace(c.BroadcastID) == "" {
		return nil
	}
	hasMedia := c.MediaURL.Valid && strings.TrimSpace(c.MediaURL.String) != ""
	if body == "" && !hasMedia {
		return nil
	}

	// Telegram caption limit is ~1024; keep it safe.
	const captionMaxRunes = 900
	caption := truncateRunes(body, captionMaxRunes)

	mt := strings.ToLower(strings.TrimSpace(c.MediaType.String))
	if hasMedia && mt == "image" {
		msg := tgbotapi.NewPhoto(c.ChatID, tgbotapi.FileURL(strings.TrimSpace(c.MediaURL.String)))
		if caption != "" {
			msg.Caption = caption
		}
		_, err := bot.Send(msg)
		return err
	}
	if hasMedia && mt == "video" {
		msg := tgbotapi.NewVideo(c.ChatID, tgbotapi.FileURL(strings.TrimSpace(c.MediaURL.String)))
		if caption != "" {
			msg.Caption = caption
		}
		_, err := bot.Send(msg)
		return err
	}
	text := caption
	if c.Title.Valid && strings.TrimSpace(c.Title.String) != "" {
		text = strings.TrimSpace(c.Title.String) + "\n\n" + caption
	}
	_, err := bot.Send(tgbotapi.NewMessage(c.ChatID, text))
	return err
}

var retryAfterRe = regexp.MustCompile(`(?i)\bretry after (\d+)\b`)

func markBroadcastDeliverySkipped(ctx context.Context, db *sql.DB, broadcastID string, chatID int64) {
	if db == nil || strings.TrimSpace(broadcastID) == "" || chatID == 0 {
		return
	}
	_, _ = db.ExecContext(ctx, `
		INSERT OR IGNORE INTO broadcast_telegram_deliveries (broadcast_id, chat_id, delivered_at)
		VALUES (?1, ?2, datetime('now'))`, broadcastID, chatID)
}

func markUserTelegramBlocked(ctx context.Context, db *sql.DB, chatID int64) {
	if db == nil || chatID == 0 {
		return
	}
	_, _ = db.ExecContext(ctx, `
		UPDATE users SET telegram_bot_blocked = 1 WHERE telegram_id = ?1`, chatID)
}

// isTelegramDeliveryPermanentErr reports Telegram send failures that will not
// succeed on retry (blocked bot, deactivated account, missing chat).
func isTelegramDeliveryPermanentErr(err error) bool {
	if err == nil {
		return false
	}
	var tgErr tgbotapi.Error
	if errors.As(err, &tgErr) && tgErr.Code == 403 {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "bot was blocked by the user") ||
		strings.Contains(msg, "user is deactivated") ||
		strings.Contains(msg, "chat not found") ||
		strings.Contains(msg, "forbidden")
}

func telegramRetryAfterSeconds(err error) int {
	if err == nil {
		return 0
	}
	// Prefer typed telegram-bot-api error when available.
	var tgErr tgbotapi.Error
	if errors.As(err, &tgErr) {
		if tgErr.ResponseParameters.RetryAfter > 0 {
			return tgErr.ResponseParameters.RetryAfter
		}
	}
	// Fallback: parse message ("Too Many Requests: retry after X").
	m := retryAfterRe.FindStringSubmatch(err.Error())
	if len(m) == 2 {
		if n, e := strconv.Atoi(strings.TrimSpace(m[1])); e == nil && n > 0 {
			return n
		}
	}
	return 0
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}
