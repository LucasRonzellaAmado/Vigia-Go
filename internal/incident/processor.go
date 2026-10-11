package incident

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/luquinha/vigia/internal/alert"
	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/monitor"
	"github.com/luquinha/vigia/internal/store"
)

type Checker interface {
	Check(ctx context.Context, t checker.Target) checker.Result
}

type Notifier interface {
	Notify(ctx context.Context, projectID int64, ev alert.Event) (int, error)
}

type Processor struct {
	db       *store.DB
	checker  Checker
	notifier Notifier
	reminder time.Duration
	baseURL  string
	logger   *slog.Logger
	now      func() time.Time
	OnResult func(m store.Monitor, res checker.Result)
	OnAlert  func(kind alert.Kind, sent int, err error)
}

func NewProcessor(db *store.DB, c Checker, n Notifier, reminder time.Duration, baseURL string, logger *slog.Logger) *Processor {
	return &Processor{
		db:       db,
		checker:  c,
		notifier: n,
		reminder: reminder,
		baseURL:  baseURL,
		logger:   logger,
		now:      time.Now,
	}
}

func (p *Processor) Run(ctx context.Context, monitorID int64) {
	m, err := p.db.GetMonitor(ctx, monitorID)
	if err != nil {
		if !store.IsNotFound(err) {
			p.logger.Error("carregar monitor", "monitor_id", monitorID, "error", err)
		}
		return
	}
	if !m.Enabled {
		return
	}

	res := p.checker.Check(ctx, monitor.TargetOf(m))
	if p.OnResult != nil {
		p.OnResult(m, res)
	}

	if err := p.Handle(ctx, m, res); err != nil {
		p.logger.Error("processar resultado", "monitor_id", m.ID, "error", err)
	}
}

func (p *Processor) Handle(ctx context.Context, m store.Monitor, res checker.Result) error {
	var (
		tr   Transition
		next State
		inc  store.Incident
		has  bool
	)

	err := p.db.InTx(ctx, func(q *store.Queries) error {
		cur, err := q.GetMonitor(ctx, m.ID)
		if err != nil {
			return fmt.Errorf("recarregar monitor: %w", err)
		}
		m = cur

		if err := q.InsertCheckResult(ctx, store.InsertCheckResultParams{
			MonitorID:  m.ID,
			CheckedAt:  res.CheckedAt,
			Ok:         res.OK,
			LatencyMs:  int32(res.Latency.Milliseconds()),
			StatusCode: int32(res.StatusCode),
			Message:    truncate(res.Message, 500),
		}); err != nil {
			return fmt.Errorf("gravar resultado: %w", err)
		}

		next, tr = Evaluate(StateOf(m), res.OK, res.CheckedAt, int(m.FailureThreshold))

		checkedAt := res.CheckedAt
		var failingSince *time.Time
		if !next.FailingSince.IsZero() {
			failingSince = &next.FailingSince
		}
		if err := q.UpdateMonitorState(ctx, store.UpdateMonitorStateParams{
			ID:                  m.ID,
			Status:              string(next.Status),
			ConsecutiveFailures: int32(next.ConsecutiveFailures),
			FailingSince:        failingSince,
			LastCheckedAt:       &checkedAt,
		}); err != nil {
			return fmt.Errorf("atualizar estado: %w", err)
		}

		switch {
		case tr == WentDown:
			inc, err = q.OpenIncident(ctx, store.OpenIncidentParams{
				MonitorID: m.ID,
				StartedAt: next.FailingSince,
				Cause:     truncate(res.Message, 500),
			})
			if err != nil {
				return fmt.Errorf("abrir incidente: %w", err)
			}
			has = true
		case tr == Recovered:
			resolvedAt := res.CheckedAt
			inc, err = q.ResolveIncident(ctx, store.ResolveIncidentParams{MonitorID: m.ID, ResolvedAt: &resolvedAt})
			if err != nil && !store.IsNotFound(err) {
				return fmt.Errorf("fechar incidente: %w", err)
			}
			has = err == nil
		case next.Status == StatusDown:
			inc, err = q.GetOpenIncident(ctx, m.ID)
			if err != nil && !store.IsNotFound(err) {
				return fmt.Errorf("buscar incidente aberto: %w", err)
			}
			has = err == nil
		}
		return nil
	})
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}

	if tr != NoChange {
		p.logger.Info("mudança de estado", "monitor_id", m.ID, "monitor", m.Name, "transition", tr.String(), "message", res.Message)
	}
	if !has {
		return nil
	}

	switch {
	case tr == WentDown:
		p.alert(ctx, m, inc, alert.KindDown, res)
	case tr == Recovered:
		p.alert(ctx, m, inc, alert.KindUp, res)
	case next.Status == StatusDown && alert.ShouldRemind(inc.LastAlertAt, int(inc.AlertsSent), p.now(), p.reminder):
		p.alert(ctx, m, inc, alert.KindReminder, res)
	}
	return nil
}

func (p *Processor) alert(ctx context.Context, m store.Monitor, inc store.Incident, kind alert.Kind, res checker.Result) {
	ev := alert.Event{
		Kind:        kind,
		MonitorID:   m.ID,
		MonitorName: m.Name,
		MonitorKind: m.Kind,
		Target:      displayTarget(m),
		ProjectID:   m.ProjectID,
		Cause:       res.Message,
		StartedAt:   inc.StartedAt,
		OccurredAt:  res.CheckedAt,
		URL:         fmt.Sprintf("%s/monitors/%d", p.baseURL, m.ID),
	}
	if inc.ResolvedAt != nil {
		ev.ResolvedAt = *inc.ResolvedAt
	}

	sent, err := p.notifier.Notify(ctx, m.ProjectID, ev)
	if p.OnAlert != nil {
		p.OnAlert(kind, sent, err)
	}
	if err != nil {
		p.logger.Warn("alerta com falhas", "monitor_id", m.ID, "kind", kind, "sent", sent, "error", err)
	}

	now := p.now().UTC()
	if err := p.db.MarkIncidentAlerted(context.WithoutCancel(ctx), store.MarkIncidentAlertedParams{ID: inc.ID, LastAlertAt: &now}); err != nil {
		p.logger.Error("marcar alerta", "incident_id", inc.ID, "error", err)
	}
}

func displayTarget(m store.Monitor) string {
	if m.Kind == string(checker.KindHeartbeat) {
		return "heartbeat"
	}
	return m.Target
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (p *Processor) SetClock(now func() time.Time) {
	p.now = now
}
