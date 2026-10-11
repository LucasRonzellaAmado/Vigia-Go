package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luquinha/vigia/internal/store"
)

type fakeSource struct {
	mu       sync.Mutex
	monitors []store.Monitor
}

func (f *fakeSource) ListEnabledMonitors(ctx context.Context) ([]store.Monitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Monitor(nil), f.monitors...), nil
}

func (f *fakeSource) set(ms ...store.Monitor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monitors = ms
}

type counter struct {
	mu         sync.Mutex
	calls      map[int64]int
	concurrent map[int64]int
	overlap    atomic.Bool
	delay      time.Duration
	running    atomic.Int32
	maxRunning atomic.Int32
}

func newCounter(delay time.Duration) *counter {
	return &counter{calls: map[int64]int{}, concurrent: map[int64]int{}, delay: delay}
}

func (c *counter) run(ctx context.Context, id int64) {
	c.mu.Lock()
	c.calls[id]++
	c.concurrent[id]++
	if c.concurrent[id] > 1 {
		c.overlap.Store(true)
	}
	c.mu.Unlock()

	n := c.running.Add(1)
	for {
		m := c.maxRunning.Load()
		if n <= m || c.maxRunning.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(c.delay)
	c.running.Add(-1)

	c.mu.Lock()
	c.concurrent[id]--
	c.mu.Unlock()
}

func (c *counter) get(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startScheduler(t *testing.T, s *Scheduler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Run retornou erro: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler não parou")
		}
	}
}

func TestRunsEachMonitorRepeatedly(t *testing.T) {
	src := &fakeSource{}
	src.set(store.Monitor{ID: 1, IntervalSeconds: 1}, store.Monitor{ID: 2, IntervalSeconds: 1})
	c := newCounter(0)

	s := New(src, c.run, Options{Workers: 2, Tick: 10 * time.Millisecond, StartJitter: time.Millisecond, Logger: quietLogger()})
	stop := startScheduler(t, s)
	time.Sleep(2300 * time.Millisecond)
	stop()

	for _, id := range []int64{1, 2} {
		if got := c.get(id); got < 2 || got > 4 {
			t.Errorf("monitor %d rodou %d vezes, esperava 2 a 4", id, got)
		}
	}
}

func TestNeverRunsSameMonitorConcurrently(t *testing.T) {
	src := &fakeSource{}
	src.set(store.Monitor{ID: 1, IntervalSeconds: 10})
	c := newCounter(150 * time.Millisecond)

	s := New(src, c.run, Options{Workers: 4, Tick: 5 * time.Millisecond, StartJitter: time.Millisecond, Logger: quietLogger()})
	stop := startScheduler(t, s)
	for range 20 {
		s.Trigger(1)
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	if c.overlap.Load() {
		t.Fatal("o mesmo monitor rodou em paralelo")
	}
	if c.get(1) < 1 {
		t.Fatal("monitor nunca rodou")
	}
}

func TestWorkerPoolLimitsConcurrency(t *testing.T) {
	src := &fakeSource{}
	var ms []store.Monitor
	for i := range 20 {
		ms = append(ms, store.Monitor{ID: int64(i + 1), IntervalSeconds: 60})
	}
	src.set(ms...)
	c := newCounter(50 * time.Millisecond)

	s := New(src, c.run, Options{Workers: 3, Tick: 5 * time.Millisecond, StartJitter: time.Millisecond, Logger: quietLogger()})
	stop := startScheduler(t, s)
	time.Sleep(8 * time.Second / 10)
	stop()

	if got := c.maxRunning.Load(); got > 3 {
		t.Fatalf("rodaram %d checagens ao mesmo tempo com 3 workers", got)
	}
	for _, m := range ms {
		if c.get(m.ID) != 1 {
			t.Errorf("monitor %d rodou %d vezes, queria 1", m.ID, c.get(m.ID))
		}
	}
}

func TestShutdownWaitsForInFlightChecks(t *testing.T) {
	src := &fakeSource{}
	src.set(store.Monitor{ID: 1, IntervalSeconds: 60})

	started := make(chan struct{})
	var finished atomic.Bool
	run := func(ctx context.Context, id int64) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		if ctx.Err() == nil {
			finished.Store(true)
		}
	}

	s := New(src, run, Options{Workers: 1, Tick: 5 * time.Millisecond, StartJitter: time.Millisecond, Logger: quietLogger()})
	stop := startScheduler(t, s)
	<-started
	stop()

	if !finished.Load() {
		t.Fatal("o shutdown deveria esperar a checagem terminar sem cancelar o contexto dela")
	}
}

func TestReloadAddsAndRemovesMonitors(t *testing.T) {
	src := &fakeSource{}
	src.set(store.Monitor{ID: 1, IntervalSeconds: 60})
	c := newCounter(0)

	s := New(src, c.run, Options{Workers: 2, Tick: 5 * time.Millisecond, ReloadEvery: time.Hour, StartJitter: time.Millisecond, Logger: quietLogger()})
	stop := startScheduler(t, s)
	defer stop()

	waitFor(t, func() bool { return c.get(1) == 1 })

	src.set(store.Monitor{ID: 2, IntervalSeconds: 60})
	s.Reload()
	waitFor(t, func() bool { return c.get(2) == 1 })
	waitFor(t, func() bool { tracked, _ := s.Stats(); return tracked == 1 })
}

func TestFirstRunRespectsLastCheck(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-20 * time.Second)
	old := now.Add(-10 * time.Minute)

	tests := []struct {
		name    string
		last    *time.Time
		wantMin time.Time
		wantMax time.Time
	}{
		{"nunca checado: logo (com jitter)", nil, now, now.Add(5 * time.Second)},
		{"checado há pouco: espera o intervalo", &recent, recent.Add(time.Minute), recent.Add(time.Minute)},
		{"checado há muito: logo", &old, now, now.Add(5 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstRun(store.Monitor{LastCheckedAt: tt.last}, time.Minute, now, 5*time.Second)
			if got.Before(tt.wantMin) || got.After(tt.wantMax) {
				t.Errorf("firstRun = %v, queria entre %v e %v", got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condição não satisfeita a tempo")
}
