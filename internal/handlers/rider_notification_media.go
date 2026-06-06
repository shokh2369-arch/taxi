package handlers

import "strings"

const (
	BroadcastContentText  = "text"
	BroadcastContentImage = "image"
	BroadcastContentVideo = "video"
)

type riderNotificationMedia struct {
	Type     string
	URL      string
	PublicID string
	Width    int
	Height   int
	Format   string
}

func buildRiderNotificationMedia(
	secureURL, publicID, mediaType, format string,
	width, height int,
) *riderNotificationMedia {
	u := strings.TrimSpace(secureURL)
	if u == "" {
		return nil
	}
	mt := strings.TrimSpace(mediaType)
	if mt == "" {
		mt = "image"
	}
	m := &riderNotificationMedia{
		Type: mt,
		URL:  u,
	}
	if pid := strings.TrimSpace(publicID); pid != "" {
		m.PublicID = pid
	}
	if width > 0 {
		m.Width = width
	}
	if height > 0 {
		m.Height = height
	}
	if f := strings.TrimSpace(format); f != "" {
		m.Format = f
	}
	return m
}

func riderNotificationContentType(media *riderNotificationMedia) string {
	if media == nil {
		return BroadcastContentText
	}
	if strings.ToLower(strings.TrimSpace(media.Type)) == BroadcastContentVideo {
		return BroadcastContentVideo
	}
	return BroadcastContentImage
}

// riderNotificationItemJSON builds a Flutter-friendly notification payload with
// both snake_case and camelCase keys for media fields.
func riderNotificationItemJSON(id, title, body, createdAt string, media *riderNotificationMedia) map[string]any {
	contentType := riderNotificationContentType(media)
	out := map[string]any{
		"id":           id,
		"type":         contentType,
		"content_type": contentType,
		"contentType":  contentType,
		"created_at":   createdAt,
		"createdAt":    createdAt,
	}
	if strings.TrimSpace(body) != "" {
		out["body"] = body
	}
	if title != "" {
		out["title"] = title
	}
	if media == nil {
		return out
	}
	mediaOut := map[string]any{
		"type": media.Type,
		"url":  media.URL,
	}
	if media.PublicID != "" {
		mediaOut["public_id"] = media.PublicID
		mediaOut["publicId"] = media.PublicID
	}
	if media.Width > 0 {
		mediaOut["width"] = media.Width
	}
	if media.Height > 0 {
		mediaOut["height"] = media.Height
	}
	if media.Format != "" {
		mediaOut["format"] = media.Format
	}
	out["media"] = mediaOut
	out["media_url"] = media.URL
	out["mediaUrl"] = media.URL

	switch contentType {
	case BroadcastContentVideo:
		out["video_url"] = media.URL
		out["videoUrl"] = media.URL
	case BroadcastContentImage:
		out["image_url"] = media.URL
		out["imageUrl"] = media.URL
	}
	return out
}
