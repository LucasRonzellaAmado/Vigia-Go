package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/luquinha/vigia/internal/store"
)

type Kind string

const (
	KindDown     Kind = "down"
	KindUp       Kind = "up"
	KindReminder Kind = "reminder"
	KindTest     Kind = "test"
)

type Event struct {
	Kind        Kind      `json:"kind"`
	MonitorID   int64     `json:"monitor_id"`
	MonitorName string    `json:"monitor_name"`
	MonitorKind string    `json:"monitor_kind"`
	Target      string    `json:"target"`
	ProjectID   int64     `json:"project_id"`
	Cause       string    `json:"cause"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	ResolvedAt  time.Time `json:"resolved_at,omitzero"`
	OccurredAt  time.Time `json:"occurred_at"`
	URL         string    `json:"url"`
}

func (e Event) Downtime() time.Duration {
	end := e.ResolvedAt
	if end.IsZero() {
		end = e.OccurredAt
	}
	if e.StartedAt.IsZero() || end.Before(e.StartedAt) {
		return 0
	}
	return end.Sub(e.StartedAt)
}

type Message struct {
	Subject string
	Text    string
	Event   Event
}

type Notifier interface {
	Send(ctx context.Context, target string, msg Message) error
}

type ChannelSource interface {
	ListEnabledChannelsForProject(ctx context.Context, projectID int64) ([]store.AlertChannel, error)
}

type Dispatcher struct {
	channels  ChannelSource
	notifiers map[string]Notifier
	loc       *time.Location
	logger    *slog.Logger
	retries   int
	backoff   time.Duration
}

func NewDispatcher(channels ChannelSource, loc *time.Location, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		channels:  channels,
		notifiers: make(map[string]Notifier),
		loc:       loc,
		logger:    logger,
		retries:   2,
		backoff:   time.Second,
	}
}

func (d *Dispatcher) Register(kind string, n Notifier) {
	d.notifiers[kind] = n
}

func (d *Dispatcher) Supports(kind string) bool {
	_, ok := d.notifiers[kind]
	return ok
}

func (d *Dispatcher) Notify(ctx context.Context, projectID int64, ev Event) (int, error) {
	chans, err := d.channels.ListEnabledChannelsForProject(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("listar canais: %w", err)
	}
	if len(chans) == 0 {
		d.logger.Debug("projeto sem canais de alerta", "project_id", projectID, "monitor_id", ev.MonitorID)
		return 0, nil
	}

	msg := Format(ev, d.loc)

	var g errgroup.Group
	g.SetLimit(4)
	sent := make(chan struct{}, len(chans))
	for _, ch := range chans {
		g.Go(func() error {
			if err := d.send(ctx, ch, msg); err != nil {
				return err
			}
			sent <- struct{}{}
			return nil
		})
	}
	err = g.Wait()
	close(sent)
	return len(sent), err
}

func (d *Dispatcher) SendTest(ctx context.Context, ch store.AlertChannel) error {
	ev := Event{
		Kind:        KindTest,
		MonitorName: "Monitor de teste",
		Target:      "https://exemplo.com",
		OccurredAt:  time.Now().UTC(),
	}
	return d.send(ctx, ch, Format(ev, d.loc))
}

func (d *Dispatcher) send(ctx context.Context, ch store.AlertChannel, msg Message) error {
	n, ok := d.notifiers[ch.Kind]
	if !ok {
		return fmt.Errorf("canal %q (%s): tipo não configurado no servidor", ch.Name, ch.Kind)
	}

	var err error
	for attempt := range d.retries + 1 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * d.backoff):
			}
		}
		sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = n.Send(sendCtx, ch.Target, msg)
		cancel()
		if err == nil {
			d.logger.Info("alerta enviado", "channel_id", ch.ID, "channel_kind", ch.Kind, "event", msg.Event.Kind, "monitor_id", msg.Event.MonitorID)
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) {
			break
		}
	}
	d.logger.Error("falha ao enviar alerta", "channel_id", ch.ID, "channel_kind", ch.Kind, "error", err)
	return fmt.Errorf("canal %q (%s): %w", ch.Name, ch.Kind, err)
}

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

func Format(ev Event, loc *time.Location) Message {
	when := func(t time.Time) string { return t.In(loc).Format("02/01/2006 15:04") }

	var subject string
	var b strings.Builder
	switch ev.Kind {
	case KindDown:
		subject = fmt.Sprintf("🔴 %s caiu", ev.MonitorName)
		fmt.Fprintf(&b, "%s\n\nAlvo: %s\nCausa: %s\nDesde: %s", subject, ev.Target, ev.Cause, when(ev.StartedAt))
	case KindUp:
		subject = fmt.Sprintf("🟢 %s voltou", ev.MonitorName)
		fmt.Fprintf(&b, "%s\n\nAlvo: %s\nFicou fora por %s (%s até %s)", subject, ev.Target, HumanDuration(ev.Downtime()), when(ev.StartedAt), when(ev.ResolvedAt))
	case KindReminder:
		subject = fmt.Sprintf("🟠 %s continua fora", ev.MonitorName)
		fmt.Fprintf(&b, "%s\n\nAlvo: %s\nFora há %s\nÚltima causa: %s", subject, ev.Target, HumanDuration(ev.Downtime()), ev.Cause)
	default:
		subject = "✅ Teste de alerta do Vigia"
		fmt.Fprintf(&b, "%s\n\nSe você recebeu esta mensagem, o canal está configurado certinho. (%s)", subject, when(ev.OccurredAt))
	}
	if ev.URL != "" {
		fmt.Fprintf(&b, "\n\n%s", ev.URL)
	}
	return Message{Subject: "[Vigia] " + subject, Text: b.String(), Event: ev}
}

func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int(d / time.Minute)
	seconds := int((d - time.Duration(minutes)*time.Minute) / time.Second)

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
}
