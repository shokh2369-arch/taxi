package services

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type BroadcastCreateInput struct {
	ID                   string
	Title                string
	Body                 string
	CreatedByTelegramID  int64
	Audience             string
	CloudinaryPublicID   string
	CloudinarySecureURL  string
	MediaType            string
	Width                int
	Height               int
	Format               string
}

func CreateBroadcastPost(ctx context.Context, db *sql.DB, in BroadcastCreateInput) (string, error) {
	if db == nil {
		return "", fmt.Errorf("broadcast: nil db")
	}
	body := strings.TrimSpace(in.Body)
	hasMedia := strings.TrimSpace(in.CloudinarySecureURL) != ""
	if body == "" && !hasMedia {
		return "", fmt.Errorf("broadcast: empty body")
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = uuid.NewString()
	}
	aud := strings.TrimSpace(in.Audience)
	if aud == "" {
		aud = "all_riders"
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO broadcast_posts (
			id, title, body, status, created_by_telegram_id, audience,
			cloudinary_public_id, cloudinary_secure_url, media_type, width, height, format
		) VALUES (
			?1, NULLIF(TRIM(?2),''), ?3, 'published', ?4, ?5,
			NULLIF(TRIM(?6),''), NULLIF(TRIM(?7),''), NULLIF(TRIM(?8),''), NULLIF(?9,0), NULLIF(?10,0), NULLIF(TRIM(?11),'')
		)
	`, id, in.Title, body, in.CreatedByTelegramID, aud,
		in.CloudinaryPublicID, in.CloudinarySecureURL, in.MediaType, in.Width, in.Height, in.Format)
	if err != nil {
		return "", fmt.Errorf("broadcast: insert: %w", err)
	}
	return id, nil
}

