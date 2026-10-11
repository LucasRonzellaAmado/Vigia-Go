package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/store"
)

var (
	ErrNotFound     = errors.New("monitor não encontrado")
	ErrLimitReached = errors.New("limite de monitores do plano atingido")
)

type Service struct {
	db       *store.DB
	onChange func()
	now      func() time.Time
}

func NewService(db *store.DB, onChange func()) *Service {
	if onChange == nil {
		onChange = func() {}
	}
	return &Service{db: db, onChange: onChange, now: time.Now}
}

func (s *Service) Changed() { s.onChange() }

func (s *Service) List(ctx context.Context, user store.User) ([]store.ListMonitorsByUserRow, error) {
	return s.db.ListMonitorsByUser(ctx, user.ID)
}

func (s *Service) Get(ctx context.Context, user store.User, id int64) (store.Monitor, error) {
	m, err := s.db.GetMonitorForUser(ctx, store.GetMonitorForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		if store.IsNotFound(err) {
			return store.Monitor{}, ErrNotFound
		}
		return store.Monitor{}, fmt.Errorf("buscar monitor: %w", err)
	}
	return m, nil
}

func (s *Service) Create(ctx context.Context, user store.User, in Input) (store.Monitor, error) {
	in.Normalize()
	limits := auth.LimitsFor(user.Plan)
	if errs := in.Validate(limits); errs != nil {
		return store.Monitor{}, errs
	}
	if err := s.checkProject(ctx, user, in.ProjectID); err != nil {
		return store.Monitor{}, err
	}

	count, err := s.db.CountMonitorsByUser(ctx, user.ID)
	if err != nil {
		return store.Monitor{}, fmt.Errorf("contar monitores: %w", err)
	}
	if int(count) >= limits.MaxMonitors {
		return store.Monitor{}, fmt.Errorf("%w (%d no plano %s)", ErrLimitReached, limits.MaxMonitors, limits.Plan.Label())
	}

	var token *string
	if in.Kind == string(checker.KindHeartbeat) {
		t := auth.RandomToken(18)
		token = &t
	}

	m, err := s.db.CreateMonitor(ctx, store.CreateMonitorParams{
		ProjectID:        in.ProjectID,
		Name:             in.Name,
		Kind:             in.Kind,
		Target:           in.Target,
		IntervalSeconds:  int32(in.IntervalSeconds),
		TimeoutSeconds:   int32(in.TimeoutSeconds),
		Retries:          int32(*in.Retries),
		FailureThreshold: int32(in.FailureThreshold),
		ExpectedStatus:   int32(in.ExpectedStatus),
		Keyword:          in.Keyword,
		SslExpiryDays:    int32(in.SSLExpiryDays),
		HeartbeatToken:   token,
		Enabled:          *in.Enabled,
	})
	if err != nil {
		return store.Monitor{}, fmt.Errorf("criar monitor: %w", err)
	}
	s.onChange()
	return m, nil
}

func (s *Service) Update(ctx context.Context, user store.User, id int64, in Input) (store.Monitor, error) {
	current, err := s.Get(ctx, user, id)
	if err != nil {
		return store.Monitor{}, err
	}
	in.Kind = current.Kind
	in.Normalize()
	if errs := in.Validate(auth.LimitsFor(user.Plan)); errs != nil {
		return store.Monitor{}, errs
	}
	if in.ProjectID != current.ProjectID {
		if err := s.checkProject(ctx, user, in.ProjectID); err != nil {
			return store.Monitor{}, err
		}
	}

	var updated store.Monitor
	err = s.db.InTx(ctx, func(q *store.Queries) error {
		updated, err = q.UpdateMonitor(ctx, store.UpdateMonitorParams{
			ID:               id,
			UserID:           user.ID,
			ProjectID:        in.ProjectID,
			Name:             in.Name,
			Target:           in.Target,
			IntervalSeconds:  int32(in.IntervalSeconds),
			TimeoutSeconds:   int32(in.TimeoutSeconds),
			Retries:          int32(*in.Retries),
			FailureThreshold: int32(in.FailureThreshold),
			ExpectedStatus:   int32(in.ExpectedStatus),
			Keyword:          in.Keyword,
			SslExpiryDays:    int32(in.SSLExpiryDays),
			Enabled:          *in.Enabled,
		})
		if err != nil {
			return err
		}
		if current.Enabled && !updated.Enabled {
			return s.pause(ctx, q, updated)
		}
		return nil
	})
	if err != nil {
		if store.IsNotFound(err) {
			return store.Monitor{}, ErrNotFound
		}
		return store.Monitor{}, fmt.Errorf("atualizar monitor: %w", err)
	}
	s.onChange()
	return updated, nil
}

func (s *Service) SetEnabled(ctx context.Context, user store.User, id int64, enabled bool) (store.Monitor, error) {
	m, err := s.Get(ctx, user, id)
	if err != nil {
		return store.Monitor{}, err
	}
	err = s.db.InTx(ctx, func(q *store.Queries) error {
		n, err := q.SetMonitorEnabled(ctx, store.SetMonitorEnabledParams{ID: id, UserID: user.ID, Enabled: enabled})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if !enabled {
			return s.pause(ctx, q, m)
		}
		return nil
	})
	if err != nil {
		return store.Monitor{}, err
	}
	s.onChange()
	return s.Get(ctx, user, id)
}

func (s *Service) pause(ctx context.Context, q *store.Queries, m store.Monitor) error {
	now := s.now().UTC()
	if _, err := q.ResolveIncident(ctx, store.ResolveIncidentParams{MonitorID: m.ID, ResolvedAt: &now}); err != nil && !store.IsNotFound(err) {
		return fmt.Errorf("fechar incidente ao pausar: %w", err)
	}
	return q.UpdateMonitorState(ctx, store.UpdateMonitorStateParams{
		ID:            m.ID,
		Status:        "unknown",
		LastCheckedAt: m.LastCheckedAt,
	})
}

func (s *Service) Delete(ctx context.Context, user store.User, id int64) error {
	n, err := s.db.DeleteMonitorForUser(ctx, store.DeleteMonitorForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		return fmt.Errorf("apagar monitor: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	s.onChange()
	return nil
}

func (s *Service) Results(ctx context.Context, user store.User, id int64, limit int) ([]store.CheckResult, error) {
	if _, err := s.Get(ctx, user, id); err != nil {
		return nil, err
	}
	limit = min(max(limit, 1), 500)
	return s.db.ListRecentResults(ctx, store.ListRecentResultsParams{MonitorID: id, MaxRows: int32(limit)})
}

func (s *Service) checkProject(ctx context.Context, user store.User, projectID int64) error {
	if _, err := s.db.GetProjectForUser(ctx, store.GetProjectForUserParams{ID: projectID, UserID: user.ID}); err != nil {
		if store.IsNotFound(err) {
			return ValidationErrors{"project_id": "projeto não encontrado"}
		}
		return fmt.Errorf("buscar projeto: %w", err)
	}
	return nil
}
