package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/luquinha/vigia/internal/store"
)

type Source interface {
	ListEnabledMonitors(ctx context.Context) ([]store.Monitor, error)
}

type RunFunc func(ctx context.Context, monitorID int64)

type Options struct {
	Workers     int
	Tick        time.Duration
	ReloadEvery time.Duration
	StartJitter time.Duration
	Logger      *slog.Logger
}

type Scheduler struct {
	source  Source
	run     RunFunc
	opts    Options
	logger  *slog.Logger
	now     func() time.Time
	reload  chan struct{}
	trigger chan int64

	mu       sync.Mutex
	inFlight int
	tracked  int
}

type entry struct {
	id       int64
	interval time.Duration
	next     time.Time
}

func New(source Source, run RunFunc, opts Options) *Scheduler {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.Tick <= 0 {
		opts.Tick = time.Second
	}
	if opts.ReloadEvery <= 0 {
		opts.ReloadEvery = 30 * time.Second
	}
	if opts.StartJitter <= 0 {
		opts.StartJitter = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Scheduler{
		source:  source,
		run:     run,
		opts:    opts,
		logger:  opts.Logger.With("component", "scheduler"),
		now:     time.Now,
		reload:  make(chan struct{}, 1),
		trigger: make(chan int64, 64),
	}
}

func (s *Scheduler) Reload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

func (s *Scheduler) Trigger(monitorID int64) {
	select {
	case s.trigger <- monitorID:
	default:
	}
}

func (s *Scheduler) Stats() (tracked, inFlight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracked, s.inFlight
}

func (s *Scheduler) Run(ctx context.Context) error {
	jobs := make(chan int64)
	done := make(chan int64, s.opts.Workers)
	workCtx := context.WithoutCancel(ctx)

	var wg sync.WaitGroup
	for range s.opts.Workers {
		wg.Go(func() {
			for id := range jobs {
				s.safeRun(workCtx, id)
				done <- id
			}
		})
	}

	entries := make(map[int64]*entry)
	busy := make(map[int64]bool)

	if err := s.load(ctx, entries); err != nil {
		s.logger.Error("carga inicial de monitores falhou", "error", err)
	}

	ticker := time.NewTicker(s.opts.Tick)
	defer ticker.Stop()
	reloadTicker := time.NewTicker(s.opts.ReloadEvery)
	defer reloadTicker.Stop()

	s.logger.Info("scheduler iniciado", "workers", s.opts.Workers, "monitores", len(entries))

	for {
		select {
		case <-ctx.Done():
			close(jobs)
			go func() {
				wg.Wait()
				close(done)
			}()
			for id := range done {
				delete(busy, id)
			}
			s.setStats(len(entries), 0)
			s.logger.Info("scheduler parado, checagens em andamento finalizadas")
			return nil

		case id := <-done:
			delete(busy, id)
			s.setStats(len(entries), len(busy))

		case <-ticker.C:
			s.dispatch(entries, busy, jobs)

		case <-reloadTicker.C:
			if err := s.load(ctx, entries); err != nil {
				s.logger.Error("recarregar monitores", "error", err)
			}

		case <-s.reload:
			if err := s.load(ctx, entries); err != nil {
				s.logger.Error("recarregar monitores", "error", err)
			}
			s.dispatch(entries, busy, jobs)

		case id := <-s.trigger:
			if e, ok := entries[id]; ok {
				e.next = s.now()
				s.dispatch(entries, busy, jobs)
			}
		}
	}
}

func (s *Scheduler) dispatch(entries map[int64]*entry, busy map[int64]bool, jobs chan<- int64) {
	now := s.now()

	due := make([]*entry, 0)
	for _, e := range entries {
		if !busy[e.id] && !e.next.After(now) {
			due = append(due, e)
		}
	}
	slices.SortFunc(due, func(a, b *entry) int { return a.next.Compare(b.next) })

	for _, e := range due {
		select {
		case jobs <- e.id:
			busy[e.id] = true
			e.next = now.Add(e.interval)
		default:
			if len(due) > 0 {
				s.logger.Debug("todos os workers ocupados, adiando checagens", "pendentes", len(due))
			}
			s.setStats(len(entries), len(busy))
			return
		}
	}
	s.setStats(len(entries), len(busy))
}

func (s *Scheduler) load(ctx context.Context, entries map[int64]*entry) error {
	monitors, err := s.source.ListEnabledMonitors(ctx)
	if err != nil {
		return fmt.Errorf("listar monitores: %w", err)
	}

	now := s.now()
	seen := make(map[int64]bool, len(monitors))
	for _, m := range monitors {
		seen[m.ID] = true
		interval := time.Duration(m.IntervalSeconds) * time.Second

		if e, ok := entries[m.ID]; ok {
			if e.interval != interval {
				e.interval = interval
				if limit := now.Add(interval); e.next.After(limit) {
					e.next = limit
				}
			}
			continue
		}
		entries[m.ID] = &entry{id: m.ID, interval: interval, next: firstRun(m, interval, now, s.opts.StartJitter)}
	}
	for id := range entries {
		if !seen[id] {
			delete(entries, id)
		}
	}
	s.setStats(len(entries), -1)
	return nil
}

func firstRun(m store.Monitor, interval time.Duration, now time.Time, maxJitter time.Duration) time.Time {
	jitter := time.Duration(rand.Int64N(int64(min(interval, maxJitter)) + 1))
	if m.LastCheckedAt != nil {
		next := m.LastCheckedAt.Add(interval)
		if next.After(now) {
			return next
		}
	}
	return now.Add(jitter)
}

func (s *Scheduler) safeRun(ctx context.Context, id int64) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic durante checagem", "monitor_id", id, "panic", r)
		}
	}()
	s.run(ctx, id)
}

func (s *Scheduler) setStats(tracked, inFlight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tracked = tracked
	if inFlight >= 0 {
		s.inFlight = inFlight
	}
}
