package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{
			name: "defaults",
			env:  map[string]string{"DATABASE_URL": "postgres://x"},
			check: func(t *testing.T, c Config) {
				if c.HTTPAddr != ":8080" {
					t.Errorf("HTTPAddr = %q", c.HTTPAddr)
				}
				if c.Workers != 16 {
					t.Errorf("Workers = %d", c.Workers)
				}
				if c.Location.String() != "America/Sao_Paulo" {
					t.Errorf("Location = %s", c.Location)
				}
				if c.LogLevel != slog.LevelInfo {
					t.Errorf("LogLevel = %v", c.LogLevel)
				}
				if c.CookieSecure {
					t.Error("CookieSecure deveria ser false para http")
				}
				if c.AlertReminder != time.Hour {
					t.Errorf("AlertReminder = %v", c.AlertReminder)
				}
			},
		},
		{
			name: "overrides",
			env: map[string]string{
				"DATABASE_URL":         "postgres://x",
				"VIGIA_BASE_URL":       "https://status.exemplo.com/",
				"VIGIA_WORKERS":        "4",
				"VIGIA_LOG_LEVEL":      "debug",
				"VIGIA_ALERT_REMINDER": "15m",
			},
			check: func(t *testing.T, c Config) {
				if c.BaseURL != "https://status.exemplo.com" {
					t.Errorf("BaseURL = %q", c.BaseURL)
				}
				if !c.CookieSecure {
					t.Error("CookieSecure deveria ser true para https")
				}
				if c.Workers != 4 || c.LogLevel != slog.LevelDebug || c.AlertReminder != 15*time.Minute {
					t.Errorf("config = %+v", c)
				}
			},
		},
		{
			name:    "missing database url",
			env:     map[string]string{},
			wantErr: "DATABASE_URL",
		},
		{
			name:    "bad int",
			env:     map[string]string{"DATABASE_URL": "x", "VIGIA_WORKERS": "muitos"},
			wantErr: "VIGIA_WORKERS",
		},
		{
			name:    "bad duration",
			env:     map[string]string{"DATABASE_URL": "x", "VIGIA_SCHEDULER_TICK": "1 segundo"},
			wantErr: "VIGIA_SCHEDULER_TICK",
		},
		{
			name:    "smtp without from",
			env:     map[string]string{"DATABASE_URL": "x", "SMTP_HOST": "smtp.exemplo.com"},
			wantErr: "SMTP_FROM",
		},
		{
			name:    "whatsapp half configured",
			env:     map[string]string{"DATABASE_URL": "x", "WHATSAPP_TOKEN": "abc"},
			wantErr: "WHATSAPP_TOKEN",
		},
		{
			name:    "bad timezone",
			env:     map[string]string{"DATABASE_URL": "x", "VIGIA_TIMEZONE": "Marte/Olympus"},
			wantErr: "VIGIA_TIMEZONE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(envFrom(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("erro = %v, queria conter %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}
