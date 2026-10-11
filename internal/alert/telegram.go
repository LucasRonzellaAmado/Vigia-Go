package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Telegram struct {
	Token   string
	BaseURL string
	Client  *http.Client
}

func (t *Telegram) Send(ctx context.Context, chatID string, msg Message) error {
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  strings.TrimSpace(chatID),
		"text":                     msg.Text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(t.BaseURL, "/"), t.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.Client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("telegram: %s: %w", urlErr.Op, urlErr.Err)
		}
		return fmt.Errorf("telegram: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(data, &body)

	if resp.StatusCode == http.StatusOK && body.OK {
		return nil
	}
	err = fmt.Errorf("telegram: status %d: %s", resp.StatusCode, body.Description)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return permanentError{err}
	}
	return err
}
