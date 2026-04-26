package admin

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/accounting"
	"taxi-mvp/internal/config"
	"taxi-mvp/internal/repositories"
	"taxi-mvp/internal/services"
)

const (
	btnFareMenu       = "💰 Нарх белгилаш"
	btnBaseFare       = "🚕 Старт нархи"
	btnTier0_1        = "1️⃣ 0–1 км нархи"
	btnTier1_2        = "2️⃣ 1–2 км нархи"
	btnTier2Plus      = "♾ 2 км дан юқори нарх"
	btnCommissionPct  = "📊 Комиссия %"
	btnViewTariff     = "📄 Жорий тарифни кўриш"
	btnBack           = "◀️ Орқага"
	btnAddPlace       = "📍 Lokatsiya qoshish"
)

type placeAddState struct {
	mu        sync.Mutex
	step      map[int64]string // telegram user id -> "name" | "location"
	tempName  map[int64]string
}

func (s *placeAddState) setStep(telegramID int64, step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step == nil {
		s.step = make(map[int64]string)
	}
	s.step[telegramID] = step
}

func (s *placeAddState) getStep(telegramID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.step[telegramID]
	return st, ok
}

func (s *placeAddState) setName(telegramID int64, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tempName == nil {
		s.tempName = make(map[int64]string)
	}
	s.tempName[telegramID] = name
}

func (s *placeAddState) getName(telegramID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.tempName[telegramID]
	return n, ok
}

func (s *placeAddState) clear(telegramID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step != nil {
		delete(s.step, telegramID)
	}
	if s.tempName != nil {
		delete(s.tempName, telegramID)
	}
}

// pendingEdit indicates which fare field the admin is editing (value is the field key).
type fareEditState struct {
	mu    sync.Mutex
	field map[int64]string // telegram user id -> "base_fare" | "tier_0_1" | "tier_1_2" | "tier_2_plus" | "commission_percent"
}

func (s *fareEditState) set(telegramID int64, field string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.field == nil {
		s.field = make(map[int64]string)
	}
	s.field[telegramID] = field
}

func (s *fareEditState) get(telegramID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.field[telegramID]
	return f, ok
}

func (s *fareEditState) clear(telegramID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.field, telegramID)
}

// Run starts the admin bot. driverBot is used to send messages to drivers (approval/reject); admin bot must not message drivers (chat not found).
func Run(ctx context.Context, cfg *config.Config, db *sql.DB, bot *tgbotapi.BotAPI, fareSvc *services.FareService, driverBot *tgbotapi.BotAPI) error {
	if cfg == nil || cfg.AdminID == 0 || fareSvc == nil {
		return nil
	}
	log.Printf("admin bot: started @%s (admin_id=%d)", bot.Self.UserName, cfg.AdminID)
	state := &fareEditState{}
	placeState := &placeAddState{}
	placeRepo := repositories.NewPlaceRepo(db)
	updates := bot.GetUpdatesChan(tgbotapi.NewUpdate(0))
	for {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			handleUpdate(bot, cfg, db, fareSvc, driverBot, state, placeState, placeRepo, update)
		}
	}
}

func handleUpdate(bot *tgbotapi.BotAPI, cfg *config.Config, db *sql.DB, fareSvc *services.FareService, driverBot *tgbotapi.BotAPI, state *fareEditState, placeState *placeAddState, placeRepo *repositories.PlaceRepo, update tgbotapi.Update) {
	// Handle callback queries (approve/reject driver verification, delete place) first.
	if update.CallbackQuery != nil {
		handleCallback(bot, cfg, db, driverBot, update.CallbackQuery, placeRepo)
		return
	}
	var chatID int64
	var fromID int64
	if update.Message != nil {
		chatID = update.Message.Chat.ID
		if update.Message.From != nil {
			fromID = update.Message.From.ID
		}
	}
	if fromID == 0 {
		return
	}
	if fromID != cfg.AdminID {
		_, _ = bot.Send(tgbotapi.NewMessage(chatID, "⛔ Сизга рухсат йўқ."))
		return
	}

	// Place add flow: location step.
	if update.Message != nil && update.Message.Location != nil {
		if st, ok := placeState.getStep(fromID); ok && st == "location" {
			name, _ := placeState.getName(fromID)
			name = strings.TrimSpace(name)
			if name == "" {
				placeState.clear(fromID)
				sendMessage(bot, chatID, "Хатолик. /add_place ни қайта ишга туширинг.")
				return
			}
			loc := update.Message.Location
			if loc == nil {
				return
			}
			if _, err := placeRepo.Create(context.Background(), name, loc.Latitude, loc.Longitude); err != nil {
				log.Printf("admin bot: create place: %v", err)
				placeState.clear(fromID)
				sendMessage(bot, chatID, "Хатолик. Сақланмади.")
				return
			}
			placeState.clear(fromID)
			sendMessage(bot, chatID, "✅ Сақланди.")
			sendMainMenu(bot, chatID)
			return
		}
	}

	// Check if we are waiting for a numeric value for a field
	if update.Message != nil && update.Message.Text != "" {
		if field, ok := state.get(fromID); ok {
			handleNumericInput(bot, cfg, fareSvc, state, chatID, fromID, update.Message.Text, field)
			return
		}
	}

	if update.Message == nil || update.Message.Text == "" {
		return
	}
	text := strings.TrimSpace(update.Message.Text)

	// Place add flow: name step.
	if st, ok := placeState.getStep(fromID); ok && st == "name" {
		name := strings.TrimSpace(text)
		if name == "" {
			sendMessage(bot, chatID, "Илтимос, жой номини киритинг.")
			return
		}
		placeState.setName(fromID, name)
		placeState.setStep(fromID, "location")
		sendMessage(bot, chatID, "Энди локацияни юборинг (📍 Location).")
		return
	}

	switch text {
	case "/start":
		placeState.clear(fromID)
		sendMainMenu(bot, chatID)
	case btnFareMenu:
		sendFareSubmenu(bot, chatID)
	case btnAddPlace:
		state.clear(fromID)
		placeState.clear(fromID)
		placeState.setStep(fromID, "name")
		sendMessage(bot, chatID, "Жой номини киритинг:")
	case btnBaseFare:
		state.set(fromID, "base_fare")
		sendMessage(bot, chatID, "Янги старт нархини киритинг (сўм):")
	case btnTier0_1:
		state.set(fromID, "tier_0_1")
		sendMessage(bot, chatID, "0–1 км учун нархни киритинг (сўм/км):")
	case btnTier1_2:
		state.set(fromID, "tier_1_2")
		sendMessage(bot, chatID, "1–2 км учун нархни киритинг (сўм/км):")
	case btnTier2Plus:
		state.set(fromID, "tier_2_plus")
		sendMessage(bot, chatID, "2 км дан юқори учун нархни киритинг (сўм/км):")
	case btnCommissionPct:
		state.set(fromID, "commission_percent")
		sendMessage(bot, chatID, "Комиссия фоизини киритинг (0–100):")
	case btnViewTariff:
		sendCurrentTariff(bot, fareSvc, chatID)
	case btnBack:
		state.clear(fromID)
		sendMainMenu(bot, chatID)
	case "/add_place":
		state.clear(fromID)
		placeState.clear(fromID)
		placeState.setStep(fromID, "name")
		sendMessage(bot, chatID, "Жой номини киритинг:")
	case "/delete_place":
		state.clear(fromID)
		placeState.clear(fromID)
		sendPlaceDeleteMenu(bot, chatID, placeRepo)
	default:
		// If not in edit state, show main menu
		state.clear(fromID)
		placeState.clear(fromID)
		sendMainMenu(bot, chatID)
	}
}

func handleCallback(bot *tgbotapi.BotAPI, cfg *config.Config, db *sql.DB, driverBot *tgbotapi.BotAPI, q *tgbotapi.CallbackQuery, placeRepo *repositories.PlaceRepo) {
	if q == nil {
		return
	}
	if strings.HasPrefix(q.Data, "place_del:") {
		handlePlaceDeleteCallback(bot, cfg, q, placeRepo)
		return
	}
	handleApprovalCallback(bot, cfg, db, driverBot, q)
}

func handleApprovalCallback(bot *tgbotapi.BotAPI, cfg *config.Config, db *sql.DB, driverBot *tgbotapi.BotAPI, q *tgbotapi.CallbackQuery) {
	if bot == nil || cfg == nil || db == nil || q == nil {
		return
	}
	// Answer callback immediately to stop retries/spam.
	if q.ID != "" {
		_, _ = bot.Request(tgbotapi.NewCallback(q.ID, ""))
	}
	if q.From == nil || q.From.ID != cfg.AdminID {
		return
	}
	data := q.Data
	if !strings.HasPrefix(data, "approve_driver_") && !strings.HasPrefix(data, "reject_driver_") {
		return
	}
	parts := strings.Split(data, "_")
	if len(parts) < 3 {
		return
	}
	driverIDStr := parts[len(parts)-1]
	driverUserID, err := strconv.ParseInt(driverIDStr, 10, 64)
	if err != nil || driverUserID <= 0 {
		return
	}
	ctx := context.Background()
	var driverTgID int64
	var currentStatus string
	var approvalNotified int
	if err := db.QueryRowContext(ctx, `
		SELECT u.telegram_id, COALESCE(d.verification_status, ''), COALESCE(d.approval_notified, 0)
		FROM drivers d JOIN users u ON u.id = d.user_id
		WHERE d.user_id = ?1`, driverUserID).Scan(&driverTgID, &currentStatus, &approvalNotified); err != nil {
		log.Printf("admin bot: load driver for verify callback user_id=%d: %v", driverUserID, err)
		return
	}
	if strings.HasPrefix(data, "approve_driver_") {
		if currentStatus == "approved" {
			// Already approved: just reflect this in admin message if possible.
			if q.Message != nil {
				edit := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID,
					fmt.Sprintf("✅ Ҳайдовчи аллақачон тасдиқланган (user_id=%d).", driverUserID))
				_, _ = bot.Request(edit)
			}
			return
		}
		// Approve driver.
		if _, err := db.ExecContext(ctx, `UPDATE drivers SET verification_status = 'approved' WHERE user_id = ?1`, driverUserID); err != nil {
			log.Printf("admin bot: approve driver update error user_id=%d: %v", driverUserID, err)
			return
		}
		if err := accounting.TryGrantSignupPromoOnce(ctx, db, driverUserID); err != nil {
			log.Printf("admin bot: signup promo grant user_id=%d: %v", driverUserID, err)
		}
		// Do not send to driver from admin bot (driver has no chat with admin bot → "chat not found").
		// Driver approval notifier sends approval + bonus + keyboard via driver bot.

		// Update admin message to show success and remove buttons.
		if q.Message != nil {
			editText := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID,
				fmt.Sprintf("✅ Ҳайдовчи тасдиқланди (user_id=%d).", driverUserID))
			_, _ = bot.Request(editText)
			clearMarkup := tgbotapi.NewEditMessageReplyMarkup(q.Message.Chat.ID, q.Message.MessageID, tgbotapi.InlineKeyboardMarkup{})
			_, _ = bot.Request(clearMarkup)
		}
		return
	}

	// reject_driver_
	if currentStatus == "approved" {
		// Already approved: reflect in admin message if possible.
		if q.Message != nil {
			edit := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID,
				fmt.Sprintf("✅ Ҳайдовчи аллақачон тасдиқланган (user_id=%d).", driverUserID))
			_, _ = bot.Request(edit)
		}
		return
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE drivers
		SET verification_status = 'rejected',
		    license_photo_file_id = NULL,
		    vehicle_doc_file_id = NULL,
		    application_step = 'license_photo'
		WHERE user_id = ?1`, driverUserID); err != nil {
		log.Printf("admin bot: reject driver update error user_id=%d: %v", driverUserID, err)
		return
	}
	if driverTgID != 0 && driverBot != nil {
		rej := tgbotapi.NewMessage(driverTgID, "❌ Ҳужжатларингиз тасдиқланмади.\nИлтимос, аниқроқ расм юборинг.")
		if _, err := driverBot.Send(rej); err != nil {
			log.Printf("admin bot: notify rejected driver via driver bot send error user_id=%d: %v", driverUserID, err)
		}
	}

	// Update admin message to show rejection and remove buttons.
	if q.Message != nil {
		editText := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID,
			fmt.Sprintf("❌ Ҳайдовчи рад этилди (user_id=%d).", driverUserID))
		_, _ = bot.Request(editText)
		clearMarkup := tgbotapi.NewEditMessageReplyMarkup(q.Message.Chat.ID, q.Message.MessageID, tgbotapi.InlineKeyboardMarkup{})
		_, _ = bot.Request(clearMarkup)
	}
}

func sendPlaceDeleteMenu(bot *tgbotapi.BotAPI, chatID int64, placeRepo *repositories.PlaceRepo) {
	if bot == nil || placeRepo == nil {
		return
	}
	ps, err := placeRepo.List(context.Background())
	if err != nil {
		sendMessage(bot, chatID, "Хатолик.")
		return
	}
	if len(ps) == 0 {
		sendMessage(bot, chatID, "Жойлар рўйхати бўш.")
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, p := range ps {
		idStr := strconv.FormatInt(p.ID, 10)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(p.Name, "place_del:"+idStr),
		))
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	m := tgbotapi.NewMessage(chatID, "Ўчириш учун жойни танланг:")
	m.ReplyMarkup = kb
	if _, err := bot.Send(m); err != nil {
		log.Printf("admin bot: send delete place menu: %v", err)
	}
}

func handlePlaceDeleteCallback(bot *tgbotapi.BotAPI, cfg *config.Config, q *tgbotapi.CallbackQuery, placeRepo *repositories.PlaceRepo) {
	// ACK quickly.
	if bot != nil && q != nil && q.ID != "" {
		_, _ = bot.Request(tgbotapi.NewCallback(q.ID, ""))
	}
	if q == nil || q.From == nil || cfg == nil || q.From.ID != cfg.AdminID || placeRepo == nil {
		return
	}
	idStr := strings.TrimPrefix(q.Data, "place_del:")
	id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil || id <= 0 {
		return
	}
	if err := placeRepo.Delete(context.Background(), id); err != nil {
		log.Printf("admin bot: delete place id=%d: %v", id, err)
		sendMessage(bot, q.Message.Chat.ID, "Хатолик. Ўчирилмади.")
		return
	}
	sendMessage(bot, q.Message.Chat.ID, "✅ Ўчирилди.")
	// Refresh list (still safe even if message was deleted).
	sendPlaceDeleteMenu(bot, q.Message.Chat.ID, placeRepo)
}

func handleNumericInput(bot *tgbotapi.BotAPI, cfg *config.Config, fareSvc *services.FareService, state *fareEditState, chatID, adminTelegramID int64, text, field string) {
	val, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		sendMessage(bot, chatID, "Илтимос, бутун сон киритинг.")
		return
	}
	if field != "commission_percent" && val < 0 {
		sendMessage(bot, chatID, "Илтимос, мусбат бутун сон киритинг (сўм).")
		return
	}
	ctx := context.Background()
	switch field {
	case "base_fare":
		_, err = fareSvc.UpdateBaseFare(ctx, val, adminTelegramID)
	case "tier_0_1":
		_, err = fareSvc.UpdateTier0To1(ctx, val, adminTelegramID)
	case "tier_1_2":
		_, err = fareSvc.UpdateTier1To2(ctx, val, adminTelegramID)
	case "tier_2_plus":
		_, err = fareSvc.UpdateTier2Plus(ctx, val, adminTelegramID)
	case "commission_percent":
		if val < 0 || val > 100 {
			sendMessage(bot, chatID, "Илтимос, 0 дан 100 гача бутун сон киритинг.")
			state.clear(adminTelegramID)
			return
		}
		_, err = fareSvc.UpdateCommissionPercent(ctx, int(val), adminTelegramID)
	default:
		state.clear(adminTelegramID)
		sendMainMenu(bot, chatID)
		return
	}
	state.clear(adminTelegramID)
	if err != nil {
		log.Printf("admin bot: update fare %s: %v", field, err)
		sendMessage(bot, chatID, "Хатолик: янгилаш амалга ошмади.")
		return
	}
	sendMessage(bot, chatID, "✅ Янгиланди.")
	sendCurrentTariff(bot, fareSvc, chatID)
	sendFareSubmenu(bot, chatID)
}

func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64) {
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnFareMenu),
			tgbotapi.NewKeyboardButton(btnAddPlace),
		),
	)
	kb.ResizeKeyboard = true
	msg := tgbotapi.NewMessage(chatID, "Админ панели. Қуйидаги тугмалардан фойдаланинг:")
	msg.ReplyMarkup = kb
	if _, err := bot.Send(msg); err != nil {
		log.Printf("admin bot: send main menu: %v", err)
	}
}

func sendFareSubmenu(bot *tgbotapi.BotAPI, chatID int64) {
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnBaseFare),
			tgbotapi.NewKeyboardButton(btnTier0_1),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnTier1_2),
			tgbotapi.NewKeyboardButton(btnTier2Plus),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnCommissionPct),
			tgbotapi.NewKeyboardButton(btnViewTariff),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(btnBack),
		),
	)
	kb.ResizeKeyboard = true
	msg := tgbotapi.NewMessage(chatID, "Нарх созламалари:")
	msg.ReplyMarkup = kb
	if _, err := bot.Send(msg); err != nil {
		log.Printf("admin bot: send fare submenu: %v", err)
	}
}

func sendCurrentTariff(bot *tgbotapi.BotAPI, fareSvc *services.FareService, chatID int64) {
	ctx := context.Background()
	settings, err := fareSvc.GetFareSettings(ctx)
	if err != nil {
		sendMessage(bot, chatID, "Тарифни ўқишда хатолик.")
		return
	}
	text := fmt.Sprintf(
		"📄 Жорий тариф:\n\n🚕 Старт нархи: %d сўм\n1️⃣ 0–1 км: %d сўм/км\n2️⃣ 1–2 км: %d сўм/км\n♾ 2+ км: %d сўм/км\n\n📊 Комиссия: %d%%",
		settings.BaseFare, settings.Tier0_1Km, settings.Tier1_2Km, settings.Tier2PlusKm, settings.CommissionPercent,
	)
	sendMessage(bot, chatID, text)
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	if _, err := bot.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		log.Printf("admin bot: send: %v", err)
	}
}
