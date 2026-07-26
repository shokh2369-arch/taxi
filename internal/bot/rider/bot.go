package rider

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"taxi-mvp/internal/abuse"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/domain"
	"taxi-mvp/internal/legal"
	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/safe"
	"taxi-mvp/internal/services"
	"taxi-mvp/internal/utils"
)

const (
	btnLocation        = "📍 Локация юбориш"
	btnCancel          = "❌ Бекор қилиш"
	btnTaxiCall        = "🚕 Такси чақириш"
	btnTaxiNew         = "🚕 Янги такси чақириш"
	btnHelp            = "ℹ️ Ёрдам"
	btnTrackDriver     = "📍 Ҳайдовчини кузатиш"
	cbRiderAcceptTerms = "rider_accept_terms"

	resumeRiderLocation    = "rider_location"
	resumeRiderTaxi        = "rider_taxi"
	resumeRiderSearchAgain = "rider_search_again"
	resumeRiderTrack       = "rider_track"

	cbDestPage   = "dest_page:"
	cbDestPlace  = "dest_place:"
	cbReqConfirm = "req_confirm:"
	cbReqChange  = "req_change:"

	btnDestPrev   = "◀️ Аввалги"
	btnDestNext   = "Кейинги ▶️"
	btnDestCustom = "📍 Бошқа манзил"
)

type destinationWebAppPayload struct {
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
	Name string  `json:"name"`
}

// Run starts the rider bot and blocks until ctx is cancelled.
// bot is the rider Telegram bot API; matchService broadcasts new requests (may be nil); tripService is used to cancel trips (may be nil).
func Run(ctx context.Context, cfg *config.Config, db *sql.DB, bot *tgbotapi.BotAPI, matchService *services.MatchService, tripService *services.TripService) error {
	log.Printf("rider bot: started @%s", bot.Self.UserName)
	setBotCommands(bot)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	// Be explicit: WebAppData arrives as a Message update.
	u.AllowedUpdates = []string{"message", "callback_query"}
	updates := bot.GetUpdatesChan(u)

	notified := &notifiedState{}
	safe.GoSupervised(ctx, "rider_poll_notify", func() { pollAndNotifyRider(ctx, bot, db, cfg, notified) })

	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			handleUpdate(bot, db, cfg, matchService, tripService, update, notified)
		}
	}
}

type notifiedState struct {
	mu       sync.Mutex
	assigned map[string]struct{}
	started  map[string]struct{}
	finished map[string]struct{}
}

func (n *notifiedState) markAssigned(requestID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.assigned[requestID]; ok {
		return false
	}
	if n.assigned == nil {
		n.assigned = make(map[string]struct{})
	}
	n.assigned[requestID] = struct{}{}
	return true
}

func (n *notifiedState) markStarted(tripID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.started[tripID]; ok {
		return false
	}
	if n.started == nil {
		n.started = make(map[string]struct{})
	}
	n.started[tripID] = struct{}{}
	return true
}

func (n *notifiedState) markFinished(tripID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.finished[tripID]; ok {
		return false
	}
	if n.finished == nil {
		n.finished = make(map[string]struct{})
	}
	n.finished[tripID] = struct{}{}
	return true
}

func handleUpdate(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, matchService *services.MatchService, tripService *services.TripService, update tgbotapi.Update, notified *notifiedState) {
	if update.CallbackQuery != nil {
		if update.CallbackQuery.From != nil && update.CallbackQuery.Message != nil {
			log.Printf("rider: update callback chat_id=%d from_id=%d data=%q", update.CallbackQuery.Message.Chat.ID, update.CallbackQuery.From.ID, update.CallbackQuery.Data)
		} else {
			log.Printf("rider: update callback data=%q", update.CallbackQuery.Data)
		}
		handleCallback(bot, db, cfg, matchService, update.CallbackQuery)
		return
	}
	if update.Message == nil {
		return
	}
	msg := update.Message
	chatID := msg.Chat.ID
	telegramID := int64(0)
	if msg.From != nil {
		telegramID = msg.From.ID
	}

	// Compact debug: confirm Render is receiving rider updates at all.
	log.Printf("rider: update message chat_id=%d from_id=%d text_len=%d has_loc=%v has_contact=%v has_webapp=%v",
		chatID, telegramID, len(strings.TrimSpace(msg.Text)), msg.Location != nil, msg.Contact != nil, msg.WebAppData != nil)

	// Debug (low-noise): WebApp confirm sends a "service-like" message with empty text.
	// Log only when text is empty and there's no other common payload, so we can confirm what's arriving.
	if strings.TrimSpace(msg.Text) == "" && msg.Location == nil && msg.Contact == nil && msg.WebAppData == nil {
		log.Printf("rider: empty_message chat_id=%d from_id=%d msg_id=%d", chatID, telegramID, msg.MessageID)
	}

	if msg.Command() == "start" {
		var referredBy *string
		if parts := strings.Fields(msg.Text); len(parts) > 1 && parts[1] != "" {
			if code := strings.TrimPrefix(parts[1], "ref_"); code != "" {
				referredBy = &code
			}
		}
		handleStart(bot, db, chatID, telegramID, referredBy)
		return
	}
	if msg.Command() == "terms" {
		sendActiveUserTerms(bot, db, chatID)
		return
	}
	if msg.Command() == "privacy" {
		sendActivePrivacy(bot, db, chatID)
		return
	}
	if msg.Command() == "cancel" {
		handleCancel(bot, db, cfg, matchService, tripService, chatID, telegramID)
		return
	}

	ctx := context.Background()
	var riderUserID int64
	_ = db.QueryRowContext(ctx, `SELECT id FROM users WHERE telegram_id = ?1`, telegramID).Scan(&riderUserID)

	if msg.Contact != nil {
		handlePhoneContact(bot, db, chatID, telegramID, msg.Contact.PhoneNumber)
		return
	}

	// Telegram Mini App web_app_data (custom destination picker).
	if msg.WebAppData != nil {
		raw := ""
		if msg.WebAppData != nil {
			raw = strings.TrimSpace(msg.WebAppData.Data)
		}
		log.Printf("rider: web_app_data chat_id=%d from_id=%d data_len=%d", chatID, telegramID, len(raw))
		if raw == "" {
			// Still return early so WebAppData messages don't fall through into other flows.
			send(bot, chatID, "❌ Манзил маълумоти келмади.\n\nИлтимос, харитадан манзилни қайта танланг.")
			return
		}
		if riderUserID == 0 {
			send(bot, chatID, "👋 Бошлаш учун /start буйруғини босинг.")
			return
		}
		handleDestinationWebAppData(bot, db, cfg, matchService, chatID, riderUserID, raw)
		return
	}

	if msg.Location != nil {
		if riderUserID == 0 {
			send(bot, chatID, "👋 Бошлаш учун /start буйруғини босинг.")
			return
		}
		if !legal.NewService(db).RiderHasActiveLegal(ctx, riderUserID) {
			lSvc := legal.NewService(db)
			_ = lSvc.SetPendingResume(ctx, riderUserID, resumeRiderLocation, fmt.Sprintf("%f,%f", msg.Location.Latitude, msg.Location.Longitude))
			sendRiderLegalScreens(bot, db, chatID)
			return
		}
		handleLocation(bot, db, cfg, matchService, chatID, telegramID, msg.Location.Latitude, msg.Location.Longitude)
		return
	}

	if msg.Text == btnTaxiCall || msg.Text == btnTaxiNew {
		if riderUserID == 0 {
			send(bot, chatID, "👋 Бошлаш учун /start буйруғини босинг.")
			return
		}
		if !legal.NewService(db).RiderHasActiveLegal(ctx, riderUserID) {
			_ = legal.NewService(db).SetPendingResume(ctx, riderUserID, resumeRiderTaxi, "")
			sendRiderLegalScreens(bot, db, chatID)
			return
		}
		handleTaxiCall(bot, db, chatID, telegramID)
		return
	}

	if msg.Text == btnTrackDriver {
		if riderUserID == 0 {
			send(bot, chatID, "👋 Бошлаш учун /start буйруғини босинг.")
			return
		}
		if !legal.NewService(db).RiderHasActiveLegal(ctx, riderUserID) {
			_ = legal.NewService(db).SetPendingResume(ctx, riderUserID, resumeRiderTrack, "")
			sendRiderLegalScreens(bot, db, chatID)
			return
		}
		handleTrackDriver(bot, db, cfg, chatID, telegramID)
		return
	}

	// Block usage until rider accepts active legal documents.
	if riderUserID == 0 || !legal.NewService(db).RiderHasActiveLegal(ctx, riderUserID) {
		if riderUserID != 0 {
			sendRiderLegalScreens(bot, db, chatID)
		} else {
			send(bot, chatID, "⚠️ Давом этиш учун аввал қоидаларни қабул қилишингиз керак.\n\n/start буйруғини босинг.")
		}
		return
	}

	if msg.Text == btnCancel {
		handleCancel(bot, db, cfg, matchService, tripService, chatID, telegramID)
		return
	}
	if msg.Text == btnHelp {
		handleHelp(bot, chatID)
		return
	}
}

func setBotCommands(bot *tgbotapi.BotAPI) {
	cmd := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: "Бош меню"},
		tgbotapi.BotCommand{Command: "cancel", Description: "Бекор қилиш"},
		tgbotapi.BotCommand{Command: "terms", Description: "Фойдаланиш қоидалари"},
		tgbotapi.BotCommand{Command: "privacy", Description: "Махфийлик сиёсати"},
	)
	if _, err := bot.Request(cmd); err != nil {
		log.Printf("rider bot: SetMyCommands: %v", err)
	}
}

func handleCallback(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, matchService *services.MatchService, q *tgbotapi.CallbackQuery) {
	if q == nil {
		return
	}
	// Always ACK callback quickly.
	_, _ = bot.Request(tgbotapi.NewCallback(q.ID, ""))
	// CallbackQuery.Message is optional per the Telegram API; all handlers below dereference
	// q.Message.Chat and q.From, so bail out (already answered) when either is missing.
	if q.Message == nil || q.From == nil {
		return
	}

	if q.Data == cbRiderAcceptTerms {
		ctx := context.Background()
		telegramID := q.From.ID
		_, _ = db.ExecContext(ctx, `
			INSERT INTO users (telegram_id, role) VALUES (?1, ?2)
			ON CONFLICT (telegram_id) DO UPDATE SET
				role = CASE WHEN users.role = 'driver' THEN users.role ELSE excluded.role END`,
			telegramID, domain.RoleRider)
		var userID int64
		if err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE telegram_id = ?1`, telegramID).Scan(&userID); err != nil || userID == 0 {
			send(bot, q.Message.Chat.ID, "❌ Профилингизни яратиб бўлмади.\n\nИлтимос, /start ни қайта босинг.")
			return
		}
		lSvc := legal.NewService(db)
		if err := lSvc.AcceptActiveForTypes(ctx, userID, []string{legal.DocUserTerms, legal.DocPrivacyPolicyUser}, "", "telegram-bot"); err != nil {
			log.Printf("rider: legal accept: %v", err)
			send(bot, q.Message.Chat.ID, "❌ Қоидаларни қабул қилишда хатолик.\n\nИлтимос, бироздан сўнг қайта уриниб кўринг.")
			return
		}
		send(bot, q.Message.Chat.ID, "✅ Қоидалар қабул қилинди.\n\nЭнди сиз бемалол буюртма беришингиз мумкин.")
		kind, payload, ok := lSvc.TakePendingResume(ctx, userID)
		if ok {
			switch kind {
			case resumeRiderLocation:
				parts := strings.Split(payload, ",")
				if len(parts) == 2 {
					lat, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
					lng, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
					if e1 == nil && e2 == nil {
						if ensureRiderPhone(bot, db, q.Message.Chat.ID, telegramID) {
							return
						}
						handleLocation(bot, db, cfg, matchService, q.Message.Chat.ID, telegramID, lat, lng)
						return
					}
				}
			case resumeRiderTaxi:
				if ensureRiderPhone(bot, db, q.Message.Chat.ID, telegramID) {
					return
				}
				handleTaxiCall(bot, db, q.Message.Chat.ID, telegramID)
				return
			case resumeRiderSearchAgain:
				if ensureRiderPhone(bot, db, q.Message.Chat.ID, telegramID) {
					return
				}
				sendLocationPrompt(bot, q.Message.Chat.ID)
				return
			case resumeRiderTrack:
				if ensureRiderPhone(bot, db, q.Message.Chat.ID, telegramID) {
					return
				}
				handleTrackDriver(bot, db, cfg, q.Message.Chat.ID, telegramID)
				return
			}
		}
		if ensureRiderPhone(bot, db, q.Message.Chat.ID, telegramID) {
			return
		}
		sendMainMenu(bot, q.Message.Chat.ID)
		return
	}

	if q.Data == "search_again" {
		ctx := context.Background()
		telegramID := q.From.ID
		var userID int64
		_ = db.QueryRowContext(ctx, `SELECT id FROM users WHERE telegram_id = ?1`, telegramID).Scan(&userID)
		if userID == 0 || !legal.NewService(db).RiderHasActiveLegal(ctx, userID) {
			if userID != 0 {
				_ = legal.NewService(db).SetPendingResume(ctx, userID, resumeRiderSearchAgain, "")
			}
			sendRiderLegalScreens(bot, db, q.Message.Chat.ID)
			return
		}
		if ensureRiderPhone(bot, db, q.Message.Chat.ID, telegramID) {
			return
		}
		sendLocationPrompt(bot, q.Message.Chat.ID)
		return
	}
	if strings.HasPrefix(q.Data, cbDestPage) {
		handleDestinationPageCallback(bot, db, cfg, q, matchService)
		return
	}
	if strings.HasPrefix(q.Data, cbDestPlace) {
		handleDestinationPlaceCallback(bot, db, cfg, q, matchService)
		return
	}
	if strings.HasPrefix(q.Data, cbReqConfirm) {
		handleRequestConfirmCallback(bot, db, cfg, q, matchService)
		return
	}
	if strings.HasPrefix(q.Data, cbReqChange) {
		handleRequestChangeCallback(bot, db, cfg, q, matchService)
		return
	}
	_ = cfg
	_ = matchService
}

func handleStart(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64, telegramID int64, referredBy *string) {
	ctx := context.Background()
	code, err := utils.GenerateReferralCode(ctx, db)
	if err != nil {
		log.Printf("rider: generate referral code: %v", err)
		send(bot, chatID, "❌ Рўйхатдан ўтишда хатолик юз берди.\n\nИлтимос, бир неча сониядан сўнг /start ни қайта босинг.")
		return
	}
	var refArg interface{}
	if referredBy != nil && *referredBy != "" {
		refArg = *referredBy
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO users (telegram_id, role, referral_code, referred_by) VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT (telegram_id) DO UPDATE SET
			role = CASE WHEN users.role = 'driver' THEN users.role ELSE excluded.role END`,
		telegramID, domain.RoleRider, code, refArg)
	if err != nil {
		log.Printf("rider: upsert user: %v", err)
		send(bot, chatID, "❌ Рўйхатдан ўтишда хатолик юз берди.\n\nИлтимос, бир неча сониядан сўнг /start ни қайта босинг.")
		return
	}
	// User unblocked the bot and returned; allow future Telegram broadcasts again.
	_, _ = db.ExecContext(ctx, `UPDATE users SET telegram_bot_blocked = 0 WHERE telegram_id = ?1`, telegramID)

	if ensureRiderPhone(bot, db, chatID, telegramID) {
		return
	}
	var userID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE telegram_id = ?1`, telegramID).Scan(&userID); err != nil || userID == 0 {
		send(bot, chatID, "❌ Профилингиз топилмади.\n\n/start ни босиб қайтадан бошланг.")
		return
	}
	if !legal.NewService(db).RiderHasActiveLegal(ctx, userID) {
		sendRiderLegalScreens(bot, db, chatID)
		return
	}
	sendMainMenu(bot, chatID)
}

func sendRiderLegalScreens(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64) {
	ctx := context.Background()
	text, err := legal.NewService(db).RiderAgreementPromptMessage(ctx)
	if err != nil {
		log.Printf("rider: legal prompt: %v", err)
		text = legal.RiderAgreementMessage
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Қабул қиламан", cbRiderAcceptTerms),
		),
	)
	m := tgbotapi.NewMessage(chatID, text)
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send legal screens: %v", err)
	}
}

func sendActiveUserTerms(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64) {
	ctx := context.Background()
	_, content, err := legal.NewService(db).ActiveDocument(ctx, legal.DocUserTerms)
	if err != nil {
		send(bot, chatID, legal.TermsFullMessage)
		return
	}
	send(bot, chatID, content)
}

func sendActivePrivacy(bot *tgbotapi.BotAPI, db *sql.DB, chatID int64) {
	ctx := context.Background()
	_, content, err := legal.NewService(db).ActiveDocument(ctx, legal.DocPrivacyPolicyUser)
	if err != nil {
		send(bot, chatID, "⚠️ Махфийлик сиёсати ҳозирча юкланмади.\n\n/start орқали қайта уриниб кўринг.")
		return
	}
	send(bot, chatID, content)
}

// sendMainMenu shows the persistent main menu: Taxi chaqirish, Yordam.
func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64) {
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnTaxiCall),
			tgbotapi.NewKeyboardButton(btnHelp),
		),
	)
	kb.ResizeKeyboard = true
	m := tgbotapi.NewMessage(chatID, "Қуйидаги тугмалардан фойдаланинг:")
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send main menu: %v", err)
	}
}

// SendMainMenuAfterFinish shows the post-trip menu: Yangi taxi chaqirish, Yordam (used by TripService).
func SendMainMenuAfterFinish(bot *tgbotapi.BotAPI, chatID int64) {
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnTaxiNew),
			tgbotapi.NewKeyboardButton(btnHelp),
		),
	)
	kb.ResizeKeyboard = true
	m := tgbotapi.NewMessage(chatID, "✅ Сафар тугади.\n\nЯнги буюртма учун қуйидаги тугмани босинг.")
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send main menu after finish: %v", err)
	}
}

func handleTaxiCall(bot *tgbotapi.BotAPI, db *sql.DB, chatID, telegramID int64) {
	if ensureRiderPhone(bot, db, chatID, telegramID) {
		return
	}
	sendLocationPrompt(bot, chatID)
}

func handleHelp(bot *tgbotapi.BotAPI, chatID int64) {
	text := "Ёрдам:\n\n" +
		"• Такси чақириш — локациянгизни юборинг, ҳайдовчи топилади.\n" +
		"• Ҳайдовчини кузатиш — сафар давомида харитада кузатинг.\n" +
		"• Бекор қилиш — сўровни ёки сафарни бекор қилиш.\n\n" +
		"/start — бош меню\n/cancel — бекор қилиш"
	kb := mainMenuReplyKeyboard()
	m := tgbotapi.NewMessage(chatID, text)
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send help: %v", err)
	}
}

func mainMenuReplyKeyboard() tgbotapi.ReplyKeyboardMarkup {
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnTaxiCall),
			tgbotapi.NewKeyboardButton(btnHelp),
		),
	)
	kb.ResizeKeyboard = true
	return kb
}

func handleTrackDriver(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, chatID, telegramID int64) {
	var userID int64
	err := db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE telegram_id = ?1`, telegramID).Scan(&userID)
	if err != nil || userID == 0 {
		send(bot, chatID, "👋 Бошлаш учун /start буйруғини босинг.")
		return
	}
	var tripID string
	err = db.QueryRowContext(context.Background(), `
		SELECT id FROM trips
		WHERE rider_user_id = ?1 AND status IN (?2, ?3, ?4)
		ORDER BY id DESC LIMIT 1`,
		userID, domain.TripStatusWaiting, domain.TripStatusArrived, domain.TripStatusStarted).Scan(&tripID)
	if err != nil || tripID == "" {
		send(bot, chatID, "ℹ️ Ҳозирда фаол сафарингиз йўқ.")
		return
	}
	if cfg == nil || cfg.RiderMapURL == "" {
		send(bot, chatID, "⚠️ Харита ҳозирча мавжуд эмас.\n\nИлтимос, кейинроқ уриниб кўринг.")
		return
	}
	url := strings.TrimSuffix(cfg.RiderMapURL, "/") + "?trip_id=" + tripID
	kb := riderMapWebAppKeyboard("📍 Харитада кузатиш", url)
	m := tgbotapi.NewMessage(chatID, "Ҳайдовчини харитада кузатиш учун тугмани босинг:")
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send track driver: %v", err)
	}
}

// riderMapWebAppKeyboard returns an inline keyboard with one Web App button (for rider map).
func riderMapWebAppKeyboard(buttonText, webAppURL string) riderMapInlineKbd {
	return riderMapInlineKbd{
		InlineKeyboard: [][]riderMapWebAppBtn{{
			{Text: buttonText, WebApp: &riderMapWebAppInfo{URL: webAppURL}},
		}},
	}
}

type riderMapInlineKbd struct {
	InlineKeyboard [][]riderMapWebAppBtn `json:"inline_keyboard"`
}
type riderMapWebAppBtn struct {
	Text   string              `json:"text"`
	WebApp *riderMapWebAppInfo `json:"web_app,omitempty"`
}
type riderMapWebAppInfo struct {
	URL string `json:"url"`
}

// ensureRiderPhone checks if rider phone exists; if not, prompts to share contact.
// Returns true if we prompted (i.e. phone is missing).
func ensureRiderPhone(bot *tgbotapi.BotAPI, db *sql.DB, chatID, telegramID int64) bool {
	var phone sql.NullString
	_ = db.QueryRowContext(context.Background(), `SELECT phone FROM users WHERE telegram_id = ?1`, telegramID).Scan(&phone)
	if phone.Valid && phone.String != "" {
		return false
	}
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButtonContact("📞 Телефон рақамини юбориш"),
		),
	)
	kb.ResizeKeyboard = true
	kb.OneTimeKeyboard = true
	m := tgbotapi.NewMessage(chatID, "Буюртма бериш учун телефон рақамингиз керак. Тугмани босиб рақамингизни юборинг.")
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send phone prompt: %v", err)
	}
	return true
}

func handlePhoneContact(bot *tgbotapi.BotAPI, db *sql.DB, chatID, telegramID int64, phone string) {
	if phone == "" {
		_ = ensureRiderPhone(bot, db, chatID, telegramID)
		return
	}
	_, err := db.ExecContext(context.Background(), `UPDATE users SET phone = ?1 WHERE telegram_id = ?2`, phone, telegramID)
	if err != nil {
		log.Printf("rider: save phone: %v", err)
	}
	send(bot, chatID, "✅ Раҳмат! Телефон рақамингиз сақланди.\n\nЭнди «🚕 Такси чақириш» тугмасини босинг.")
	sendMainMenu(bot, chatID)
}

func sendLocationPrompt(bot *tgbotapi.BotAPI, chatID int64) {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButtonLocation(btnLocation),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = true
	m := tgbotapi.NewMessage(chatID, "Локациянгизни юборинг.")
	m.ReplyMarkup = keyboard
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send: %v", err)
	}
}

func handleLocation(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, matchService *services.MatchService, chatID, telegramID int64, lat, lng float64) {
	if ensureRiderPhone(bot, db, chatID, telegramID) {
		return
	}
	var userID int64
	ctx := context.Background()
	err := db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE telegram_id = ?1`, telegramID).Scan(&userID)
	if err != nil {
		if err == sql.ErrNoRows {
			send(bot, chatID, "👋 Бошлаш учун /start буйруғини босинг.")
			return
		}
		log.Printf("rider: get user: %v", err)
		send(bot, chatID, "❌ Маълумотларингизни олишда хатолик.\n\nИлтимос, бироздан сўнг қайта уриниб кўринг.")
		return
	}

	// Anti-abuse: block new requests while rider is temporarily blocked.
	if penalty, err := abuse.CheckRiderBlock(ctx, db, userID, time.Now()); err == nil && penalty != nil && penalty.BlockUntil != nil {
		remaining := abuse.FormatRemaining(*penalty.BlockUntil, time.Now())
		text := "⏳ Буюртма вақтинча чекланган\n\n" +
			"Кўп маротаба буюртмани бекор қилганингиз сабабли сиз вақтинча янги буюртма бера олмайсиз.\n\n" +
			"⏱ Қайта уриниб кўриш вақти: " + remaining + "\n\n" +
			"Илтимос, ҳайдовчилар вақтини ҳурмат қилинг."
		send(bot, chatID, text)
		return
	}

	// Rate limit: only 1 active (PENDING) ride request per rider
	var existing int
	if err := db.QueryRowContext(context.Background(), `SELECT 1 FROM ride_requests WHERE rider_user_id = ?1 AND status = ?2 LIMIT 1`, userID, domain.RequestStatusPending).Scan(&existing); err == nil {
		send(bot, chatID, "ℹ️ Сизда аллақачон фаол сўров бор.\n\nҲайдовчи топилишини кутинг ёки «❌ Бекор қилиш» тугмасини босинг.")
		return
	}

	requestID := uuid.New()
	expiresAt := time.Now().Add(time.Duration(cfg.RequestExpiresSeconds) * time.Second)
	pickupGrid := utils.GridID(lat, lng)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO ride_requests (id, rider_user_id, pickup_lat, pickup_lng, radius_km, status, expires_at, pickup_grid)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)`,
		requestID.String(), userID, lat, lng, cfg.MatchRadiusKm, domain.RequestStatusPending, expiresAt, pickupGrid)
	if err != nil {
		// The check above is a read followed by a write, so a fast double-tap can
		// slip past it; the unique index (migration 063) catches the rest.
		if services.IsUniqueConstraintErr(err) {
			send(bot, chatID, "ℹ️ Сизда аллақачон фаол сўров бор.\n\nҲайдовчи топилишини кутинг ёки «❌ Бекор қилиш» тугмасини босинг.")
			return
		}
		log.Printf("rider: create request: %v", err)
		send(bot, chatID, "❌ Сўров юборилмади. Илтимос, бироздан сўнг қайта уриниб кўринг.")
		return
	}

	// New flow: rider must choose destination before dispatch.
	sendDestinationPage(bot, db, cfg, chatID, userID, requestID.String(), 1, 0)
}

func handleDestinationPageCallback(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, q *tgbotapi.CallbackQuery, matchService *services.MatchService) {
	parts := strings.Split(strings.TrimPrefix(q.Data, cbDestPage), ":")
	if len(parts) != 2 {
		return
	}
	requestID := strings.TrimSpace(parts[0])
	page, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	if page <= 0 {
		page = 1
	}
	var riderUserID int64
	_ = db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE telegram_id = ?1`, q.From.ID).Scan(&riderUserID)
	if riderUserID == 0 {
		return
	}
	if q.Message == nil {
		return
	}
	sendDestinationPage(bot, db, cfg, q.Message.Chat.ID, riderUserID, requestID, page, q.Message.MessageID)
	_ = matchService
}

func handleDestinationPlaceCallback(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, q *tgbotapi.CallbackQuery, matchService *services.MatchService) {
	// dest_place:<request_id>:<place_id>
	parts := strings.Split(strings.TrimPrefix(q.Data, cbDestPlace), ":")
	if len(parts) != 2 {
		return
	}
	requestID := strings.TrimSpace(parts[0])
	placeID, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if requestID == "" || placeID <= 0 {
		return
	}
	var riderUserID int64
	_ = db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE telegram_id = ?1`, q.From.ID).Scan(&riderUserID)
	if riderUserID == 0 {
		return
	}
	// Load pickup and place coordinates.
	var pickupLat, pickupLng float64
	var st string
	if err := db.QueryRowContext(context.Background(), `SELECT pickup_lat, pickup_lng, status FROM ride_requests WHERE id = ?1 AND rider_user_id = ?2`, requestID, riderUserID).
		Scan(&pickupLat, &pickupLng, &st); err != nil || st != domain.RequestStatusPending {
		return
	}
	var name string
	var dropLat, dropLng float64
	if err := db.QueryRowContext(context.Background(), `SELECT name, lat, lng FROM places WHERE id = ?1`, placeID).Scan(&name, &dropLat, &dropLng); err != nil {
		send(bot, q.Message.Chat.ID, "❌ Танланган манзил топилмади.\n\nИлтимос, рўйхатдан бошқа манзилни танланг.")
		return
	}
	estPrice := estimatePrice(context.Background(), db, cfg, pickupLat, pickupLng, dropLat, dropLng)
	// Reset TTL from confirmation moment (selection step), so users can browse destination without expiring.
	ttl := "+120 seconds"
	if cfg != nil && cfg.RequestExpiresSeconds > 0 {
		ttl = fmt.Sprintf("+%d seconds", cfg.RequestExpiresSeconds)
	}
	_, _ = db.ExecContext(context.Background(), `
		UPDATE ride_requests
		SET drop_lat = ?1, drop_lng = ?2, drop_name = ?3, estimated_price = ?4, expires_at = datetime('now', ?5)
		WHERE id = ?6 AND rider_user_id = ?7 AND status = ?8`,
		dropLat, dropLng, strings.TrimSpace(name), estPrice, ttl, requestID, riderUserID, domain.RequestStatusPending)

	sendRiderEstimateConfirm(bot, q.Message.Chat.ID, requestID, estPrice)
}

func handleDestinationWebAppData(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, matchService *services.MatchService, chatID int64, riderUserID int64, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		send(bot, chatID, "❌ Манзил маълумоти келмади.\n\nИлтимос, харитадан манзилни қайта танланг.")
		return
	}
	var p destinationWebAppPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		send(bot, chatID, "❌ Манзилни ўқиб бўлмади.\n\nИлтимос, харитадан манзилни қайта танланг.")
		return
	}
	if math.IsNaN(p.Lat) || math.IsNaN(p.Lng) || p.Lat == 0 || p.Lng == 0 {
		send(bot, chatID, "❌ Манзил нотўғри танланган.\n\nИлтимос, харитадан аниқ нуқтани белгиланг.")
		return
	}
	// Update the latest pending request that doesn't have a destination yet.
	var requestID string
	var pickupLat, pickupLng float64
	err := db.QueryRowContext(context.Background(), `
		SELECT id, pickup_lat, pickup_lng
		FROM ride_requests
		WHERE rider_user_id = ?1 AND status = ?2 AND (drop_lat IS NULL OR drop_lng IS NULL)
		ORDER BY created_at DESC LIMIT 1`,
		riderUserID, domain.RequestStatusPending).Scan(&requestID, &pickupLat, &pickupLng)
	if err != nil || requestID == "" {
		// The Mini App writes the destination over HTTP (POST /rider/request/destination),
		// which already replies with the price prompt. Its sendData then also reaches
		// the bot here, where the request no longer matches "pending with no
		// destination" — so this used to fire "no active request found" immediately
		// after the price prompt, and riders believed the newer, contradictory
		// message and started over into the duplicate-request block.
		var pendingWithDrop int
		if e := db.QueryRowContext(context.Background(), `
			SELECT 1 FROM ride_requests
			WHERE rider_user_id = ?1 AND status = ?2
			  AND drop_lat IS NOT NULL AND drop_lng IS NOT NULL
			LIMIT 1`,
			riderUserID, domain.RequestStatusPending).Scan(&pendingWithDrop); e == nil {
			// Destination already recorded by the HTTP path; it owns the reply.
			return
		}
		send(bot, chatID, "ℹ️ Фаол сўровингиз топилмади.\n\n«🚕 Такси чақириш» орқали янги сўров юборинг.")
		return
	}
	estPrice := estimatePrice(context.Background(), db, cfg, pickupLat, pickupLng, p.Lat, p.Lng)
	name := strings.TrimSpace(p.Name)
	ttl := "+120 seconds"
	if cfg != nil && cfg.RequestExpiresSeconds > 0 {
		ttl = fmt.Sprintf("+%d seconds", cfg.RequestExpiresSeconds)
	}
	_, _ = db.ExecContext(context.Background(), `
		UPDATE ride_requests
		SET drop_lat = ?1, drop_lng = ?2, drop_name = ?3, estimated_price = ?4, expires_at = datetime('now', ?5)
		WHERE id = ?6 AND rider_user_id = ?7 AND status = ?8`,
		p.Lat, p.Lng, name, estPrice, ttl, requestID, riderUserID, domain.RequestStatusPending)

	sendRiderEstimateConfirm(bot, chatID, requestID, estPrice)
}

func sendRiderEstimateConfirm(bot *tgbotapi.BotAPI, chatID int64, requestID string, estPrice int64) {
	text := fmt.Sprintf("💰 Тахминий нарх: %s сўм\n\nТасдиқлайсизми?", utils.FormatSoM(estPrice))
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Тасдиқлаш", cbReqConfirm+requestID),
			tgbotapi.NewInlineKeyboardButtonData("◀️ Ўзгартириш", cbReqChange+requestID),
		),
	)
	m := tgbotapi.NewMessage(chatID, text)
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send estimate confirm: %v", err)
	}
}

func handleRequestChangeCallback(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, q *tgbotapi.CallbackQuery, matchService *services.MatchService) {
	requestID := strings.TrimSpace(strings.TrimPrefix(q.Data, cbReqChange))
	var riderUserID int64
	_ = db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE telegram_id = ?1`, q.From.ID).Scan(&riderUserID)
	if riderUserID == 0 || requestID == "" {
		return
	}
	// Allow changing destination only until confirmed: clear previous destination + estimate.
	_, _ = db.ExecContext(context.Background(), `
		UPDATE ride_requests
		SET drop_lat = NULL, drop_lng = NULL, drop_name = NULL, estimated_price = 0, destination_confirmed = 0
		WHERE id = ?1 AND rider_user_id = ?2 AND status = ?3`,
		requestID, riderUserID, domain.RequestStatusPending)
	sendDestinationPage(bot, db, cfg, q.Message.Chat.ID, riderUserID, requestID, 1, 0)
	_ = matchService
}

func handleRequestConfirmCallback(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, q *tgbotapi.CallbackQuery, matchService *services.MatchService) {
	requestID := strings.TrimSpace(strings.TrimPrefix(q.Data, cbReqConfirm))
	if requestID == "" {
		return
	}
	var riderUserID int64
	_ = db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE telegram_id = ?1`, q.From.ID).Scan(&riderUserID)
	if riderUserID == 0 {
		return
	}
	// Ensure destination + estimate exist and request is still valid.
	var est int64
	var st string
	err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(estimated_price, 0), status
		FROM ride_requests
		WHERE id = ?1 AND rider_user_id = ?2 AND expires_at > datetime('now')`,
		requestID, riderUserID).Scan(&est, &st)
	if err != nil || st != domain.RequestStatusPending || est <= 0 {
		send(bot, q.Message.Chat.ID, "⏳ Сўров муддати тугаган ёки нарх ҳисобланмаган.\n\n«🚕 Такси чақириш» орқали янги сўров юборинг.")
		return
	}
	// Lock destination so it cannot be changed after confirmation, and restart the
	// dispatch window: expires_at was set when the destination was picked, so time
	// spent reading the price estimate was being taken out of the search window.
	confirmTTL := "+120 seconds"
	if cfg != nil && cfg.RequestExpiresSeconds > 0 {
		confirmTTL = fmt.Sprintf("+%d seconds", cfg.RequestExpiresSeconds)
	}
	_, _ = db.ExecContext(context.Background(), `
		UPDATE ride_requests SET destination_confirmed = 1, expires_at = datetime('now', ?4)
		WHERE id = ?1 AND rider_user_id = ?2 AND status = ?3`,
		requestID, riderUserID, domain.RequestStatusPending, confirmTTL)
	if matchService != nil {
		if err := matchService.BroadcastRequest(context.Background(), requestID); err != nil {
			log.Printf("rider: broadcast request: %v", err)
		}
	}
	send(bot, q.Message.Chat.ID, "✅ Сўров кетди. Ҳозир яқин ҳайдовчиларга юбордим.")
	sendCancelKeyboard(bot, q.Message.Chat.ID)
}

func sendCancelKeyboard(bot *tgbotapi.BotAPI, chatID int64) {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnCancel),
		),
	)
	keyboard.ResizeKeyboard = true
	m := tgbotapi.NewMessage(chatID, "Ҳайдовчи топилгунча кутинг. Бекор қилиш тугмасини босишингиз мумкин.")
	m.ReplyMarkup = keyboard
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send: %v", err)
	}
}

// destinationKbd is used to include WebApp buttons (tgbotapi lacks web_app in this version).
type destinationKbd struct {
	InlineKeyboard [][]destinationBtn `json:"inline_keyboard"`
}
type destinationBtn struct {
	Text         string             `json:"text"`
	CallbackData string             `json:"callback_data,omitempty"`
	WebApp       *destinationWebApp `json:"web_app,omitempty"`
}
type destinationWebApp struct {
	URL string `json:"url"`
}

// sendDestinationPage shows the destination picker. When editMessageID > 0 the
// existing inline-keyboard message is updated (Prev/Next pagination) instead of
// sending a new chat message.
func sendDestinationPage(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, chatID int64, riderUserID int64, requestID string, page int, editMessageID int) {
	const (
		radiusKm = 10.0
		perPage  = 5
	)
	if page <= 0 {
		page = 1
	}
	var pickupLat, pickupLng float64
	var st string
	if err := db.QueryRowContext(context.Background(), `SELECT pickup_lat, pickup_lng, status FROM ride_requests WHERE id = ?1 AND rider_user_id = ?2`, requestID, riderUserID).
		Scan(&pickupLat, &pickupLng, &st); err != nil || st != domain.RequestStatusPending {
		send(bot, chatID, "❌ Сўров топилмади ёки муддати тугаган.\n\n«🚕 Такси чақириш» орқали янги сўров юборинг.")
		return
	}
	placeSvc := services.NewPlaceService(repositories.NewPlaceRepo(db))
	nearest, err := placeSvc.NearestWithin(context.Background(), pickupLat, pickupLng, radiusKm)
	if err != nil {
		log.Printf("rider: nearest places: %v", err)
		nearest = nil
	}
	// Pagination over nearest slice.
	total := len(nearest)
	maxPage := int(math.Ceil(float64(total) / float64(perPage)))
	if maxPage <= 0 {
		maxPage = 1
	}
	if page > maxPage {
		page = maxPage
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	var rows [][]destinationBtn
	for _, it := range nearest[start:end] {
		rows = append(rows, []destinationBtn{{
			Text:         it.Place.Name,
			CallbackData: cbDestPlace + requestID + ":" + strconv.FormatInt(it.Place.ID, 10),
		}})
	}
	// Bottom controls: always show Prev/Next and Custom Location.
	prevPage := page - 1
	nextPage := page + 1
	if prevPage < 1 {
		prevPage = 1
	}
	if nextPage > maxPage {
		nextPage = maxPage
	}
	rows = append(rows, []destinationBtn{
		{Text: btnDestPrev, CallbackData: cbDestPage + requestID + ":" + strconv.Itoa(prevPage)},
		{Text: btnDestNext, CallbackData: cbDestPage + requestID + ":" + strconv.Itoa(nextPage)},
	})
	rows = append(rows, []destinationBtn{
		{Text: btnDestCustom, WebApp: &destinationWebApp{URL: buildDestinationPickerURL(cfg, pickupLat, pickupLng, requestID)}},
	})
	kb := destinationKbd{InlineKeyboard: rows}
	text := "Манзилни танланг:"
	if editMessageID > 0 {
		if err := editDestinationMessage(bot, chatID, editMessageID, text, kb); err != nil && !isMessageNotModifiedErr(err) {
			log.Printf("rider: edit destination page: %v", err)
		}
		return
	}
	m := tgbotapi.NewMessage(chatID, text)
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send destination page: %v", err)
	}
}

func editDestinationMessage(bot *tgbotapi.BotAPI, chatID int64, messageID int, text string, kb destinationKbd) error {
	kbJSON, err := json.Marshal(kb)
	if err != nil {
		return err
	}
	params := tgbotapi.Params{}
	params.AddNonZero64("chat_id", chatID)
	params.AddNonZero("message_id", messageID)
	params.AddNonEmpty("text", text)
	params.AddNonEmpty("reply_markup", string(kbJSON))
	_, err = bot.MakeRequest("editMessageText", params)
	return err
}

func isMessageNotModifiedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "message is not modified")
}

func buildDestinationPickerURL(cfg *config.Config, pickupLat, pickupLng float64, requestID string) string {
	base := strings.TrimSuffix(strings.TrimSpace(os.Getenv("RIDER_PICKER_WEBAPP_URL")), "/")
	if base == "" {
		base = strings.TrimSuffix(strings.TrimSpace(os.Getenv("CUSTOM_LOCATION_WEBAPP_URL")), "/")
	}
	if base == "" && cfg != nil {
		base = strings.TrimSuffix(cfg.WebAppURL, "/")
	}
	// Do NOT break existing WEBAPP_URL behavior; this is only a best-effort fallback.
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/pick-destination.html?mode=drop&request_id=%s&pickup_lat=%f&pickup_lng=%f", base, requestID, pickupLat, pickupLng)
}

func estimatePrice(ctx context.Context, db *sql.DB, cfg *config.Config, pickupLat, pickupLng, dropLat, dropLng float64) int64 {
	// Prefer real road-route distance (OSRM). Always fallback to Haversine to avoid breaking flow.
	distanceKm := 0.0
	if route, err := services.GetRouteDistance(pickupLat, pickupLng, dropLat, dropLng); err == nil && route != nil && route.DistanceMeters > 0 {
		distanceKm = route.DistanceMeters / 1000
	} else {
		distanceKm = utils.HaversineMeters(pickupLat, pickupLng, dropLat, dropLng) / 1000
	}
	if distanceKm < 0 {
		distanceKm = 0
	}
	// Prefer existing FareService tiered settings when possible (no new pricing system).
	fareSvc := services.NewFareService(db, cfg)
	if fareSvc != nil {
		if v, err := fareSvc.CalculateFare(ctx, distanceKm); err == nil && v > 0 {
			return v
		}
	}
	// Fallback to config/env defaults (legacy).
	startingFee := 4000
	pricePerKm := 1500
	if cfg != nil {
		startingFee = cfg.StartingFee
		pricePerKm = cfg.PricePerKm
	}
	return utils.CalculateFareRounded(float64(startingFee), float64(pricePerKm), distanceKm)
}

func handleCancel(bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, matchService *services.MatchService, tripService *services.TripService, chatID, telegramID int64) {
	_ = cfg
	ctx := context.Background()
	var userID int64
	err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE telegram_id = ?1`, telegramID).Scan(&userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return
		}
		log.Printf("rider: get user: %v", err)
		send(bot, chatID, "❌ Маълумотларингизни олишда хатолик.\n\nИлтимос, қайта уриниб кўринг.")
		return
	}
	// If rider has an active trip (WAITING or STARTED), cancel the trip first ("safarni bekor qilish").
	if tripService != nil {
		var tripID string
		err := db.QueryRowContext(ctx, `
			SELECT id FROM trips
			WHERE rider_user_id = ?1 AND status IN (?2, ?3, ?4)
			ORDER BY id DESC LIMIT 1`,
			userID, domain.TripStatusWaiting, domain.TripStatusArrived, domain.TripStatusStarted).Scan(&tripID)
		if err == nil && tripID != "" {
			result, err := tripService.CancelByRider(ctx, tripID, userID)
			if err != nil {
				log.Printf("rider: cancel trip: %v", err)
				send(bot, chatID, "❌ Сафарни бекор қилиб бўлмади.\n\nИлтимос, қайта уриниб кўринг.")
				return
			}
			if result != nil {
				send(bot, chatID, "✅ Сафар бекор қилинди.")
				if ensureRiderPhone(bot, db, chatID, telegramID) {
					return
				}
				sendMainMenu(bot, chatID)
				return
			}
		}
	}
	var requestID string
	if err := db.QueryRowContext(ctx, `
		SELECT id FROM ride_requests
		WHERE rider_user_id = ?1 AND status = ?2
		ORDER BY created_at DESC LIMIT 1`,
		userID, domain.RequestStatusPending).Scan(&requestID); err != nil && err != sql.ErrNoRows {
		log.Printf("rider: cancel request lookup: %v", err)
		send(bot, chatID, "❌ Сўровни текширишда хатолик.\n\nИлтимос, қайта уриниб кўринг.")
		return
	}
	if requestID == "" {
		send(bot, chatID, "ℹ️ Бекор қилинадиган фаол сўров ёки сафар йўқ.")
		return
	}
	res, err := db.ExecContext(ctx, `UPDATE ride_requests SET status = ?1 WHERE id = ?2 AND status = ?3`,
		domain.RequestStatusCancelled, requestID, domain.RequestStatusPending)
	if err != nil {
		log.Printf("rider: cancel request: %v", err)
		send(bot, chatID, "❌ Сўровни бекор қилиб бўлмади.\n\nИлтимос, қайта уриниб кўринг.")
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		send(bot, chatID, "ℹ️ Бекор қилинадиган фаол сўров ёки сафар йўқ.")
		return
	}
	// Close driver offers like the app cancel path: delete Telegram offer messages,
	// drop SENT request_notifications rows, and poke native drivers to refetch.
	if matchService != nil {
		matchService.CleanupCancelledRequestOffers(ctx, requestID)
	}
	send(bot, chatID, "✅ Сўров бекор қилинди.")
	if ensureRiderPhone(bot, db, chatID, telegramID) {
		return
	}
	sendMainMenu(bot, chatID)
}

func pollAndNotifyRider(ctx context.Context, bot *tgbotapi.BotAPI, db *sql.DB, cfg *config.Config, notified *notifiedState) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			notifyTripUpdates(bot, db, notified)
		}
	}
}

// notifyTripUpdates is unused: trip lifecycle (start/finish) is notified by services.TripService.
func notifyTripUpdates(bot *tgbotapi.BotAPI, db *sql.DB, notified *notifiedState) {}

func formatSummary(km float64, fareAmount int64) string {
	return fmt.Sprintf("✅ Сафар тугади.\n\n📏 %s\n💰 Нарх: %s сўм", formatKm(km), utils.FormatSoM(fareAmount))
}

func formatKm(km float64) string {
	return fmt.Sprintf("%.2f км", km)
}

func send(bot *tgbotapi.BotAPI, chatID int64, text string) {
	m := tgbotapi.NewMessage(chatID, text)
	if _, err := bot.Send(m); err != nil {
		log.Printf("rider: send to %d: %v", chatID, err)
	}
}
