package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luquinha/vigia/internal/store"
)

func TestShouldRemind(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	tests := []struct {
		name      string
		lastAlert *time.Time
		sent      int
		base      time.Duration
		want      bool
	}{
		{"lembrete desligado", ago(48 * time.Hour), 1, 0, false},
		{"nunca alertou: manda", nil, 0, time.Hour, true},
		{"primeiro lembrete antes de 1h: segura", ago(59 * time.Minute), 1, time.Hour, false},
		{"primeiro lembrete com 1h: manda", ago(time.Hour), 1, time.Hour, true},
		{"segundo lembrete espera 2h", ago(90 * time.Minute), 2, time.Hour, false},
		{"segundo lembrete com 2h: manda", ago(2 * time.Hour), 2, time.Hour, true},
		{"terceiro lembrete espera 4h", ago(3 * time.Hour), 3, time.Hour, false},
		{"backoff limitado a 24h", ago(24 * time.Hour), 20, time.Hour, true},
		{"backoff limitado a 24h, ainda não", ago(23 * time.Hour), 20, time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldRemind(tt.lastAlert, tt.sent, now, tt.base); got != tt.want {
				t.Errorf("ShouldRemind = %v, queria %v", got, tt.want)
			}
		})
	}
}

func TestReminderGap(t *testing.T) {
	tests := []struct {
		sent int
		want time.Duration
	}{
		{0, 30 * time.Minute},
		{1, 30 * time.Minute},
		{2, time.Hour},
		{3, 2 * time.Hour},
		{6, 16 * time.Hour},
		{7, 24 * time.Hour},
		{1000, 24 * time.Hour},
	}
	for _, tt := range tests {
		if got := ReminderGap(tt.sent, 30*time.Minute); got != tt.want {
			t.Errorf("ReminderGap(%d) = %v, queria %v", tt.sent, got, tt.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{12*time.Minute + 30*time.Second, "12m30s"},
		{time.Hour + 5*time.Minute, "1h05m"},
		{50 * time.Hour, "2d 2h"},
	}
	for _, tt := range tests {
		if got := HumanDuration(tt.in); got != tt.want {
			t.Errorf("HumanDuration(%v) = %q, queria %q", tt.in, got, tt.want)
		}
	}
}

func TestFormat(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	start := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	ev := Event{
		Kind:        KindUp,
		MonitorName: "API",
		Target:      "https://api.exemplo.com",
		StartedAt:   start,
		ResolvedAt:  start.Add(12*time.Minute + 30*time.Second),
		URL:         "http://localhost:8080/monitors/1",
	}
	msg := Format(ev, loc)
	for _, want := range []string{"API voltou", "12m30s", "02/10/2026 10:00", "/monitors/1"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("mensagem sem %q:\n%s", want, msg.Text)
		}
	}
	if !strings.HasPrefix(msg.Subject, "[Vigia]") {
		t.Errorf("Subject = %q", msg.Subject)
	}
}

func TestTelegramSend(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		if got["chat_id"] == "bloqueado" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"ok":false,"description":"Forbidden: bot was blocked by the user"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{Token: "123:ABC", BaseURL: srv.URL, Client: srv.Client()}
	if err := tg.Send(context.Background(), " 42 ", Message{Text: "oi"}); err != nil {
		t.Fatal(err)
	}
	if path != "/bot123:ABC/sendMessage" || got["chat_id"] != "42" || got["text"] != "oi" {
		t.Fatalf("path=%s payload=%v", path, got)
	}

	err := tg.Send(context.Background(), "bloqueado", Message{Text: "oi"})
	var perm permanentError
	if !errors.As(err, &perm) || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("esperava erro permanente, veio %v", err)
	}
}

func TestWebhookSend(t *testing.T) {
	var payload webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wh := &Webhook{Client: srv.Client()}
	err := wh.Send(context.Background(), srv.URL, Message{Subject: "s", Text: "t", Event: Event{Kind: KindDown, MonitorID: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Event.MonitorID != 7 || payload.Event.Kind != KindDown || payload.Text != "t" {
		t.Fatalf("payload = %+v", payload)
	}
}

type fakeChannels []store.AlertChannel

func (f fakeChannels) ListEnabledChannelsForProject(ctx context.Context, projectID int64) ([]store.AlertChannel, error) {
	return f, nil
}

type recorder struct {
	mu      sync.Mutex
	targets []string
	fail    atomic.Int32
}

func (r *recorder) Send(ctx context.Context, target string, msg Message) error {
	if r.fail.Load() > 0 {
		r.fail.Add(-1)
		return errors.New("instável")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append(r.targets, target)
	return nil
}

func TestDispatcherNotify(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	chans := fakeChannels{
		{ID: 1, Kind: "telegram", Name: "tg", Target: "chat-1", Enabled: true},
		{ID: 2, Kind: "webhook", Name: "hook", Target: "https://hook", Enabled: true},
		{ID: 3, Kind: "whatsapp", Name: "zap", Target: "5519999999999", Enabled: true},
	}
	tg := &recorder{}
	tg.fail.Store(1)
	wh := &recorder{}

	d := NewDispatcher(chans, time.UTC, logger)
	d.backoff = time.Millisecond
	d.Register("telegram", tg)
	d.Register("webhook", wh)

	sent, err := d.Notify(context.Background(), 1, Event{Kind: KindDown, MonitorName: "x"})
	if sent != 2 {
		t.Errorf("enviados = %d, queria 2", sent)
	}
	if err == nil || !strings.Contains(err.Error(), "não configurado") {
		t.Errorf("esperava erro do canal whatsapp sem configuração, veio %v", err)
	}
	if len(tg.targets) != 1 || tg.targets[0] != "chat-1" {
		t.Errorf("telegram deveria ter enviado após retry: %v", tg.targets)
	}
	if len(wh.targets) != 1 {
		t.Errorf("webhook: %v", wh.targets)
	}
}
