package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type Webhook struct {
	Client *http.Client
}

type webhookPayload struct {
	Event   Event  `json:"event"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

func (w *Webhook) Send(ctx context.Context, url string, msg Message) error {
	payload, err := json.Marshal(webhookPayload{Event: msg.Event, Subject: msg.Subject, Text: msg.Text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return permanentError{fmt.Errorf("webhook: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Vigia-Webhook/1.0")

	resp, err := w.Client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("webhook: status %d", resp.StatusCode)
}
