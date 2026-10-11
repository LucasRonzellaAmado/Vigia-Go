package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type Config struct {
	Env             string
	HTTPAddr        string
	BaseURL         string
	DatabaseURL     string
	DBMaxConns      int32
	MigrateOnStart  bool
	LogLevel        slog.Level
	LogFormat       string
	Location        *time.Location
	Workers         int
	SchedulerTick   time.Duration
	ReloadEvery     time.Duration
	ShutdownTimeout time.Duration
	ResultRetention time.Duration
	AlertReminder   time.Duration
	SessionTTL      time.Duration
	CookieSecure    bool
	AllowSignup     bool
	MetricsToken    string

	TelegramBotToken string
	TelegramAPIURL   string

	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string

	WhatsAppToken   string
	WhatsAppPhoneID string
	WhatsAppAPIURL  string
}

func (c Config) IsProduction() bool {
	return c.Env == "production"
}

func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}

	cfg := Config{
		Env:              p.str("VIGIA_ENV", "development"),
		HTTPAddr:         p.str("VIGIA_HTTP_ADDR", ":8080"),
		BaseURL:          strings.TrimRight(p.str("VIGIA_BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:      p.str("DATABASE_URL", ""),
		DBMaxConns:       int32(p.int("VIGIA_DB_MAX_CONNS", 10)),
		MigrateOnStart:   p.bool("VIGIA_MIGRATE_ON_START", true),
		LogFormat:        p.str("VIGIA_LOG_FORMAT", "json"),
		Workers:          p.int("VIGIA_WORKERS", 16),
		SchedulerTick:    p.duration("VIGIA_SCHEDULER_TICK", time.Second),
		ReloadEvery:      p.duration("VIGIA_RELOAD_EVERY", 30*time.Second),
		ShutdownTimeout:  p.duration("VIGIA_SHUTDOWN_TIMEOUT", 30*time.Second),
		ResultRetention:  p.duration("VIGIA_RESULT_RETENTION", 30*24*time.Hour),
		AlertReminder:    p.duration("VIGIA_ALERT_REMINDER", time.Hour),
		SessionTTL:       p.duration("VIGIA_SESSION_TTL", 30*24*time.Hour),
		AllowSignup:      p.bool("VIGIA_ALLOW_SIGNUP", true),
		MetricsToken:     p.str("VIGIA_METRICS_TOKEN", ""),
		TelegramBotToken: p.str("TELEGRAM_BOT_TOKEN", ""),
		TelegramAPIURL:   p.str("TELEGRAM_API_URL", "https://api.telegram.org"),
		SMTPHost:         p.str("SMTP_HOST", ""),
		SMTPPort:         p.int("SMTP_PORT", 587),
		SMTPUser:         p.str("SMTP_USER", ""),
		SMTPPassword:     p.str("SMTP_PASSWORD", ""),
		SMTPFrom:         p.str("SMTP_FROM", ""),
		WhatsAppToken:    p.str("WHATSAPP_TOKEN", ""),
		WhatsAppPhoneID:  p.str("WHATSAPP_PHONE_NUMBER_ID", ""),
		WhatsAppAPIURL:   p.str("WHATSAPP_API_URL", "https://graph.facebook.com/v21.0"),
	}

	cfg.CookieSecure = p.bool("VIGIA_COOKIE_SECURE", strings.HasPrefix(cfg.BaseURL, "https://"))

	if err := cfg.LogLevel.UnmarshalText([]byte(p.str("VIGIA_LOG_LEVEL", "info"))); err != nil {
		p.fail("VIGIA_LOG_LEVEL", err)
	}

	loc, err := time.LoadLocation(p.str("VIGIA_TIMEZONE", "America/Sao_Paulo"))
	if err != nil {
		p.fail("VIGIA_TIMEZONE", err)
	}
	cfg.Location = loc

	if cfg.DatabaseURL == "" {
		p.fail("DATABASE_URL", errors.New("obrigatória"))
	}
	if _, err := url.ParseRequestURI(cfg.BaseURL); err != nil {
		p.fail("VIGIA_BASE_URL", err)
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		p.fail("VIGIA_LOG_FORMAT", errors.New(`use "json" ou "text"`))
	}
	if cfg.Workers < 1 {
		p.fail("VIGIA_WORKERS", errors.New("precisa ser >= 1"))
	}
	if cfg.SchedulerTick <= 0 {
		p.fail("VIGIA_SCHEDULER_TICK", errors.New("precisa ser > 0"))
	}
	if cfg.SMTPHost != "" && cfg.SMTPFrom == "" {
		p.fail("SMTP_FROM", errors.New("obrigatória quando SMTP_HOST está definida"))
	}
	if (cfg.WhatsAppToken == "") != (cfg.WhatsAppPhoneID == "") {
		p.fail("WHATSAPP_TOKEN", errors.New("WHATSAPP_TOKEN e WHATSAPP_PHONE_NUMBER_ID andam juntas"))
	}

	if len(p.errs) > 0 {
		return Config{}, fmt.Errorf("configuração inválida: %w", errors.Join(p.errs...))
	}
	return cfg, nil
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(key string, err error) {
	p.errs = append(p.errs, fmt.Errorf("%s: %w", key, err))
}

func (p *parser) str(key, def string) string {
	if v := strings.TrimSpace(p.getenv(key)); v != "" {
		return v
	}
	return def
}

func (p *parser) int(key string, def int) int {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.fail(key, err)
		return def
	}
	return n
}

func (p *parser) bool(key string, def bool) bool {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail(key, err)
		return def
	}
	return b
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := p.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail(key, err)
		return def
	}
	return d
}
