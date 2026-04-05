package services

import (
	"context"
	"database/sql"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"taxi-mvp/internal/accounting"
	"taxi-mvp/internal/driverloc"
)

// RunDriverApprovalNotifier periodically checks for drivers whose verification_status is 'approved'
// but approval_notified = 0, sends them approval and bonus info via the driver bot, and marks them notified.
func RunDriverApprovalNotifier(ctx context.Context, db *sql.DB, driverBot *tgbotapi.BotAPI) {
	if driverBot == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			notifyApprovedDrivers(ctx, db, driverBot)
		}
	}
}

func notifyApprovedDrivers(ctx context.Context, db *sql.DB, driverBot *tgbotapi.BotAPI) {
	rows, err := db.QueryContext(ctx, `
		SELECT u.telegram_id, d.user_id
		FROM drivers d
		JOIN users u ON u.id = d.user_id
		WHERE COALESCE(d.verification_status, '') = 'approved'
		  AND COALESCE(d.approval_notified, 0) = 0`)
	if err != nil {
		log.Printf("driver_approval_notifier: query: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var telegramID int64
		var userID int64
		if err := rows.Scan(&telegramID, &userID); err != nil || telegramID == 0 {
			continue
		}

		if err := accounting.TryGrantSignupPromoOnce(ctx, db, userID); err != nil {
			log.Printf("driver_approval_notifier: signup promo user_id=%d: %v", userID, err)
		}

		// 1) Profil tasdiqlandi xabari
		msg := tgbotapi.NewMessage(telegramID, "🎉 Профилингиз тасдиқланди.\n\nБуюртмалар олиш учун Telegramда жонли локацияни уланг — бошқа «онлайн» тугмаси йўқ.\n\nВидео қўлланмалар: https://t.me/+iD_MYyWnntE1NmMy")
		if _, err := driverBot.Send(msg); err != nil {
			log.Printf("driver_approval_notifier: send approved message user_id=%d: %v", userID, err)
			continue
		}

		// 2) Bonuslar haqida xabar (agar hali yuborilmagan bo'lsa)
		var welcomeSent int
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(welcome_bonus_message_sent, 0) FROM drivers WHERE user_id = ?1`, userID).Scan(&welcomeSent); err == nil && welcomeSent == 0 {
			welcome := tgbotapi.NewMessage(telegramID, accounting.DriverNewPromoProgramMessage)
			if _, err := driverBot.Send(welcome); err != nil {
				log.Printf("driver_approval_notifier: send welcome bonus message user_id=%d: %v", userID, err)
			} else {
				_, _ = db.ExecContext(ctx, `UPDATE drivers SET welcome_bonus_message_sent = 1 WHERE user_id = ?1`, userID)
			}
		}

		// 3) Reply keyboard: live-location help only (online/offline follow Telegram live share).
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				driverloc.ReplyKeyboardButtonShareLiveLocation(),
			),
		)
		kb.ResizeKeyboard = true
		keyboardMsg := tgbotapi.NewMessage(telegramID, "Қуйидаги тугмалардан фойдаланинг:")
		keyboardMsg.ReplyMarkup = kb
		if _, err := driverBot.Send(keyboardMsg); err != nil {
			log.Printf("driver_approval_notifier: send keyboard user_id=%d: %v", userID, err)
		}

		// Mark as notified so we don't resend.
		_, _ = db.ExecContext(ctx, `UPDATE drivers SET approval_notified = 1 WHERE user_id = ?1`, userID)
	}
	if err := rows.Err(); err != nil {
		log.Printf("driver_approval_notifier: rows: %v", err)
	}
}
