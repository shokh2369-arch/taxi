package handlers

import "testing"

func TestRiderNotificationItemJSON_Image(t *testing.T) {
	out := riderNotificationItemJSON("b1", "Title", "Hello", "2026-05-10T12:00:00Z", &riderNotificationMedia{
		Type: "image",
		URL:  "https://res.cloudinary.com/demo/image.jpg",
	})
	if out["image_url"] != "https://res.cloudinary.com/demo/image.jpg" {
		t.Fatalf("image_url=%v", out["image_url"])
	}
	if out["imageUrl"] != "https://res.cloudinary.com/demo/image.jpg" {
		t.Fatalf("imageUrl=%v", out["imageUrl"])
	}
	if _, ok := out["video_url"]; ok {
		t.Fatalf("video_url should be omitted for image")
	}
	media, ok := out["media"].(map[string]any)
	if !ok || media["type"] != "image" {
		t.Fatalf("media=%#v", out["media"])
	}
}

func TestRiderNotificationItemJSON_Video(t *testing.T) {
	out := riderNotificationItemJSON("b2", "", "Clip", "2026-05-10T12:00:00Z", &riderNotificationMedia{
		Type: "video",
		URL:  "https://res.cloudinary.com/demo/video.mp4",
	})
	if out["video_url"] != "https://res.cloudinary.com/demo/video.mp4" {
		t.Fatalf("video_url=%v", out["video_url"])
	}
	if out["videoUrl"] != "https://res.cloudinary.com/demo/video.mp4" {
		t.Fatalf("videoUrl=%v", out["videoUrl"])
	}
	if _, ok := out["image_url"]; ok {
		t.Fatalf("image_url should be omitted for video")
	}
	if out["mediaUrl"] != "https://res.cloudinary.com/demo/video.mp4" {
		t.Fatalf("mediaUrl=%v", out["mediaUrl"])
	}
}
