//go:build integration

package incident_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/luquinha/vigia/internal/alert"
	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/incident"
	"github.com/luquinha/vigia/internal/monitor"
	"github.com/luquinha/vigia/internal/store"
	"github.com/luquinha/vigia/internal/store/storetest"
)

type scriptedChecker struct {
	mu      sync.Mutex
	results []bool
	clock   *fakeClock
}

func (s *scriptedChecker) Check(ctx context.Context, t checker.Target) checker.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := s.results[0]
	s.results = s.results[1:]
	res := checker.Result{OK: ok, Latency: 42 * time.Millisecond, CheckedAt: s.clock.Now().UTC()}
	if ok {
		res.StatusCode, res.Message = 200, "HTTP 200"
	} else {
		res.StatusCode, res.Message = 503, "status 503 (esperado 2xx/3xx)"
	}
	return res
}

type recordingNotifier struct {
	mu     sync.Mutex
	events []alert.Event
}

func (r *recordingNotifier) Notify(ctx context.Context, projectID int64, ev alert.Event) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return 1, nil
}

func (r *recordingNotifier) kinds() []alert.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []alert.Kind
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T) (*store.DB, store.User, *monitor.Service) {
	t.Helper()
	db := storetest.New(t)
	authSvc := auth.NewService(db, time.Hour, false, true)
	user, err := authSvc.Signup(context.Background(), "lucas@exemplo.com", "senha-forte")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	return db, user, monitor.NewService(db, nil)
}

func TestIncidentLifecycle(t *testing.T) {
	ctx := context.Background()
	db, user, monitors := setup(t)

	projects, err := db.ListProjectsByUser(ctx, user.ID)
	if err != nil || len(projects) != 1 {
		t.Fatalf("signup deveria criar um projeto: %v %v", projects, err)
	}

	m, err := monitors.Create(ctx, user, monitor.Input{
		ProjectID: projects[0].ID, Name: "API", Kind: "http", Target: "https://api.exemplo.com",
		FailureThreshold: 3,
	})
	if err != nil {
		t.Fatalf("criar monitor: %v", err)
	}

	clock := &fakeClock{t: time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)}
	chk := &scriptedChecker{clock: clock, results: []bool{true, false, false, false, false, false, true}}
	notifier := &recordingNotifier{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := incident.NewProcessor(db, chk, notifier, time.Hour, "http://vigia.local", logger)
	p.SetClock(clock.Now)

	step := func(wantStatus string, wantFailures int32, wantAlerts []alert.Kind) {
		t.Helper()
		p.Run(ctx, m.ID)
		got, err := db.GetMonitor(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != wantStatus || got.ConsecutiveFailures != wantFailures {
			t.Fatalf("status=%s falhas=%d, queria %s/%d", got.Status, got.ConsecutiveFailures, wantStatus, wantFailures)
		}
		if k := notifier.kinds(); len(k) != len(wantAlerts) {
			t.Fatalf("alertas = %v, queria %v", k, wantAlerts)
		}
		clock.Advance(time.Minute)
	}

	step("up", 0, nil)
	firstFailure := clock.Now()
	step("up", 1, nil)
	step("up", 2, nil)
	step("down", 3, []alert.Kind{alert.KindDown})

	inc, err := db.GetOpenIncident(ctx, m.ID)
	if err != nil {
		t.Fatalf("incidente deveria estar aberto: %v", err)
	}
	if !inc.StartedAt.Equal(firstFailure) {
		t.Errorf("incidente deveria começar na primeira falha (%v), começou %v", firstFailure, inc.StartedAt)
	}
	if inc.AlertsSent != 1 || inc.LastAlertAt == nil {
		t.Errorf("alerta não marcado: %+v", inc)
	}

	step("down", 4, []alert.Kind{alert.KindDown})

	clock.Advance(time.Hour)
	step("down", 5, []alert.Kind{alert.KindDown, alert.KindReminder})

	step("up", 0, []alert.Kind{alert.KindDown, alert.KindReminder, alert.KindUp})

	if _, err := db.GetOpenIncident(ctx, m.ID); !store.IsNotFound(err) {
		t.Fatalf("incidente deveria estar fechado, err=%v", err)
	}
	up := notifier.events[2]
	if up.Downtime() <= time.Hour {
		t.Errorf("duração do incidente no alerta de volta = %v", up.Downtime())
	}

	results, err := db.ListRecentResults(ctx, store.ListRecentResultsParams{MonitorID: m.ID, MaxRows: 100})
	if err != nil || len(results) != 7 {
		t.Fatalf("resultados gravados = %d, err=%v", len(results), err)
	}
	stats, err := db.LatencyStats(ctx, store.LatencyStatsParams{MonitorID: m.ID, Since: clock.Now().Add(-24 * time.Hour)})
	if err != nil || stats.Checks != 7 || stats.P50 != 42 {
		t.Fatalf("latência = %+v, err=%v", stats, err)
	}
}

func TestPauseClosesOpenIncident(t *testing.T) {
	ctx := context.Background()
	db, user, monitors := setup(t)
	projects, _ := db.ListProjectsByUser(ctx, user.ID)

	m, err := monitors.Create(ctx, user, monitor.Input{
		ProjectID: projects[0].ID, Name: "Banco", Kind: "tcp", Target: "db.exemplo.com:5432", FailureThreshold: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	clock := &fakeClock{t: time.Now().UTC()}
	chk := &scriptedChecker{clock: clock, results: []bool{false}}
	p := incident.NewProcessor(db, chk, &recordingNotifier{}, time.Hour, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.SetClock(clock.Now)
	p.Run(ctx, m.ID)

	if _, err := db.GetOpenIncident(ctx, m.ID); err != nil {
		t.Fatalf("deveria haver incidente aberto: %v", err)
	}

	paused, err := monitors.SetEnabled(ctx, user, m.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Enabled || paused.Status != "unknown" || paused.ConsecutiveFailures != 0 {
		t.Fatalf("monitor pausado = %+v", paused)
	}
	if _, err := db.GetOpenIncident(ctx, m.ID); !store.IsNotFound(err) {
		t.Fatalf("pausar deveria fechar o incidente, err=%v", err)
	}
}

func TestPlanLimitsAndOwnership(t *testing.T) {
	ctx := context.Background()
	db, user, monitors := setup(t)
	projects, _ := db.ListProjectsByUser(ctx, user.ID)

	limits := auth.LimitsFor(user.Plan)
	for i := range limits.MaxMonitors {
		_, err := monitors.Create(ctx, user, monitor.Input{ProjectID: projects[0].ID, Name: "m", Kind: "ping", Target: "10.0.0." + string(rune('1'+i%9))})
		if err != nil {
			t.Fatalf("monitor %d: %v", i, err)
		}
	}
	if _, err := monitors.Create(ctx, user, monitor.Input{ProjectID: projects[0].ID, Name: "extra", Kind: "ping", Target: "10.0.0.1"}); err == nil {
		t.Fatal("deveria barrar no limite do plano free")
	}

	authSvc := auth.NewService(db, time.Hour, false, true)
	other, err := authSvc.Signup(ctx, "outra@exemplo.com", "senha-forte")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := monitors.List(ctx, user)
	if _, err := monitors.Get(ctx, other, list[0].ID); err != monitor.ErrNotFound {
		t.Fatalf("outro usuário não pode ver monitor alheio, err=%v", err)
	}
	if err := monitors.Delete(ctx, other, list[0].ID); err != monitor.ErrNotFound {
		t.Fatalf("outro usuário não pode apagar monitor alheio, err=%v", err)
	}
	if _, err := monitors.Create(ctx, other, monitor.Input{ProjectID: projects[0].ID, Name: "invasor", Kind: "ping", Target: "1.1.1.1"}); err == nil {
		t.Fatal("não pode criar monitor em projeto de outro usuário")
	}
}

func TestDuplicateSignupAndSlug(t *testing.T) {
	ctx := context.Background()
	db, user, _ := setup(t)
	authSvc := auth.NewService(db, time.Hour, false, true)

	if _, err := authSvc.Signup(ctx, "LUCAS@exemplo.com", "outra-senha"); err != auth.ErrEmailTaken {
		t.Fatalf("e-mail repetido deveria falhar com ErrEmailTaken, veio %v", err)
	}

	p1, err := auth.CreateProjectWithUniqueSlug(ctx, db.Queries, user.ID, "Loja", "loja", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := auth.CreateProjectWithUniqueSlug(ctx, db.Queries, user.ID, "Loja 2", "loja", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Slug == p2.Slug {
		t.Fatalf("slugs deveriam ser diferentes: %s", p1.Slug)
	}

	if _, err := authSvc.Authenticate(ctx, "lucas@exemplo.com", "senha-forte"); err != nil {
		t.Fatalf("login válido falhou: %v", err)
	}
	if _, err := authSvc.Authenticate(ctx, "lucas@exemplo.com", "errada"); err != auth.ErrInvalidCredentials {
		t.Fatalf("senha errada deveria falhar, veio %v", err)
	}
}
