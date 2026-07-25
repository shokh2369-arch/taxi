package cloudinary

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	CloudName string
	APIKey    string
	APISecret string
	HTTP      *http.Client
}

type UploadResult struct {
	SecureURL    string `json:"secure_url"`
	PublicID     string `json:"public_id"`
	ResourceType string `json:"resource_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Format       string `json:"format"`
}

func (c *Client) Enabled() bool {
	return strings.TrimSpace(c.CloudName) != "" && strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.APISecret) != ""
}

// UploadBytes uploads a file to Cloudinary via signed upload.
// resourceType is "image" (recommended default), "video", or "raw".
func (c *Client) UploadBytes(ctx context.Context, resourceType, folder, filename string, data []byte) (*UploadResult, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("cloudinary: not configured")
	}
	resourceType = strings.TrimSpace(resourceType)
	if resourceType == "" {
		resourceType = "image"
	}
	ts := time.Now().Unix()
	params := map[string]string{
		"timestamp": strconv.FormatInt(ts, 10),
	}
	folder = strings.TrimSpace(folder)
	if folder != "" {
		params["folder"] = folder
	}
	sig := sign(params, c.APISecret)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("api_key", c.APIKey)
	_ = w.WriteField("timestamp", params["timestamp"])
	if folder != "" {
		_ = w.WriteField("folder", folder)
	}
	_ = w.WriteField("signature", sig)

	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("cloudinary: form file: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("cloudinary: copy: %w", err)
	}
	_ = w.Close()

	url := fmt.Sprintf("https://api.cloudinary.com/v1_1/%s/%s/upload", c.CloudName, resourceType)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return nil, fmt.Errorf("cloudinary: request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudinary: do: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cloudinary: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out UploadResult
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("cloudinary: decode: %w", err)
	}
	if strings.TrimSpace(out.SecureURL) == "" || strings.TrimSpace(out.PublicID) == "" {
		return nil, fmt.Errorf("cloudinary: missing secure_url/public_id")
	}
	return &out, nil
}

func sign(params map[string]string, secret string) string {
	// Cloudinary signature: sort params by key, join as key=value&..., append API secret, sha1 hex.
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "file" || k == "signature" || k == "api_key" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	base := strings.Join(parts, "&") + secret
	h := sha1.Sum([]byte(base))
	return hex.EncodeToString(h[:])
}
