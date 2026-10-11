package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type WhatsApp struct {
	Token         string
	PhoneNumberID string
	BaseURL       string
	Client        *http.Client
}

func (w *WhatsApp) Send(ctx context.Context, phone string, msg Message) error {
	to := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	if to == "" {
		return permanentError{fmt.Errorf("whatsapp: telefone inválido %q", phone)}
	}

	payload, err := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]any{"body": msg.Text, "preview_url": false},
	})
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s/%s/messages", strings.TrimRight(w.BaseURL, "/"), w.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.Token)

	resp, err := w.Client.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err = fmt.Errorf("whatsapp: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return permanentError{err}
	}
	return err
}
