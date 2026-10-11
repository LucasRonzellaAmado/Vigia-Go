package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/luquinha/vigia/internal/alert"
	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/config"
	"github.com/luquinha/vigia/internal/incident"
	"github.com/luquinha/vigia/internal/metrics"
	"github.com/luquinha/vigia/internal/monitor"
	"github.com/luquinha/vigia/internal/scheduler"
	"github.com/luquinha/vigia/internal/statuspage"
	"github.com/luquinha/vigia/internal/store"
	"github.com/luquinha/vigia/internal/web"
)

var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "vigia:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "healthcheck":
		return healthcheck(getenv)
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	logger := newLogger(cfg, stdout)

	switch cmd {
	case "serve":
		return serve(ctx, cfg, logger)
	case "migrate":
		direction := "up"
		if len(args) > 1 {
			direction = args[1]
		}
		db, err := store.Open(ctx, cfg.DatabaseURL, 2)
		if err != nil {
			return err
		}
		defer db.Close()
		return store.Migrate(ctx, db.Pool, direction, logger)
	case "set-plan":
		if len(args) != 3 {
			return errors.New("uso: vigia set-plan <email> <free|pro>")
		}
		return setPlan(ctx, cfg, args[1], args[2], stdout)
	default:
		return fmt.Errorf("comando desconhecido %q (use serve, migrate, set-plan, healthcheck ou version)", cmd)
	}
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler = slog.NewJSONHandler(w, opts)
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h).With("app", "vigia", "version", version)
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	if cfg.MigrateOnStart {
		if err := store.Migrate(ctx, db.Pool, "up", logger); err != nil {
			return err
		}
	}

	httpClient := &http.Client{Timeout: 20 * time.Second}
	dispatcher := alert.NewDispatcher(db, cfg.Location, logger.With("component", "alert"))
	dispatcher.Register("webhook", &alert.Webhook{Client: httpClient})
	if cfg.TelegramBotToken != "" {
		dispatcher.Register("telegram", &alert.Telegram{Token: cfg.TelegramBotToken, BaseURL: cfg.TelegramAPIURL, Client: httpClient})
	}
	if cfg.SMTPHost != "" {
		dispatcher.Register("email", &alert.Email{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom})
	}
	if cfg.WhatsAppToken != "" {
		dispatcher.Register("whatsapp", &alert.WhatsApp{Token: cfg.WhatsAppToken, PhoneNumberID: cfg.WhatsAppPhoneID, BaseURL: cfg.WhatsAppAPIURL, Client: httpClient})
	}

	processor := incident.NewProcessor(db, checker.New(), dispatcher, cfg.AlertReminder, cfg.BaseURL, logger.With("component", "engine"))
	sched := scheduler.New(db, processor.Run, scheduler.Options{
		Workers:     cfg.Workers,
		Tick:        cfg.SchedulerTick,
		ReloadEvery: cfg.ReloadEvery,
		Logger:      logger,
	})

	collector := metrics.New(db, sched, cfg.MetricsToken, version, logger)
	processor.OnResult = collector.ObserveCheck
	processor.OnAlert = collector.ObserveAlert

	authSvc := auth.NewService(db, cfg.SessionTTL, cfg.CookieSecure, cfg.AllowSignup)
	monitorSvc := monitor.NewService(db, sched.Reload)

	panel, err := web.NewHandler(db, authSvc, monitorSvc, dispatcher, cfg.Location, cfg.BaseURL, logger)
	if err != nil {
		return err
	}
	status, err := statuspage.New(db, cfg.Location, cfg.BaseURL, logger)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(pingCtx); err != nil {
			http.Error(w, "database indisponível", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready\n"))
	})
	mux.Handle("GET /metrics", collector)
	monitor.NewHeartbeat(db, sched.Trigger, logger).Register(mux)
	monitor.NewAPI(monitorSvc, db, logger).Register(mux, auth.RequireUser)
	status.Register(mux)
	panel.Register(mux)

	var handler http.Handler = mux
	handler = authSvc.LoadUser(handler)
	handler = status.CustomDomains(handler)
	handler = http.NewCrossOriginProtection().Handler(handler)
	handler = web.SecurityHeaders(handler)
	handler = web.Recover(logger, handler)
	handler = web.Logging(logger, handler)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("http ouvindo", "addr", cfg.HTTPAddr, "base_url", cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		<-gctx.Done()
		logger.Info("desligando: parando de aceitar requisições")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), cfg.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})

	g.Go(func() error {
		return sched.Run(gctx)
	})

	g.Go(func() error {
		return housekeeping(gctx, db, cfg.ResultRetention, logger)
	})

	err = g.Wait()
	logger.Info("vigia parado")
	return err
}

func housekeeping(ctx context.Context, db *store.DB, retention time.Duration, logger *slog.Logger) error {
	clean := func() {
		before := time.Now().Add(-retention).UTC()
		if n, err := db.DeleteResultsBefore(ctx, before); err != nil {
			if ctx.Err() == nil {
				logger.Error("limpeza de resultados", "error", err)
			}
		} else if n > 0 {
			logger.Info("resultados antigos apagados", "rows", n)
		}
		if n, err := db.DeleteExpiredSessions(ctx); err != nil {
			if ctx.Err() == nil {
				logger.Error("limpeza de sessões", "error", err)
			}
		} else if n > 0 {
			logger.Info("sessões expiradas apagadas", "rows", n)
		}
	}

	clean()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			clean()
		}
	}
}

func setPlan(ctx context.Context, cfg config.Config, email, plan string, stdout io.Writer) error {
	if plan != string(auth.PlanFree) && plan != string(auth.PlanPro) {
		return fmt.Errorf("plano inválido %q (use free ou pro)", plan)
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := db.GetUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		if store.IsNotFound(err) {
			return fmt.Errorf("usuário %s não encontrado", email)
		}
		return err
	}
	if err := db.SetUserPlan(ctx, store.SetUserPlanParams{ID: user.ID, Plan: plan}); err != nil {
		return fmt.Errorf("atualizar plano: %w", err)
	}
	fmt.Fprintf(stdout, "%s agora está no plano %s\n", user.Email, auth.LimitsFor(plan).Plan.Label())
	return nil
}

func healthcheck(getenv func(string) string) error {
	addr := getenv("VIGIA_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("VIGIA_HTTP_ADDR inválido: %w", err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz respondeu %d", resp.StatusCode)
	}
	return nil
}
