package monitor

import (
	"testing"
	"time"

	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/store"
)

func intp(n int) *int { return &n }

func TestValidate(t *testing.T) {
	free := auth.LimitsFor("free")
	pro := auth.LimitsFor("pro")

	base := func(kind, target string) Input {
		in := Input{ProjectID: 1, Name: "Meu site", Kind: kind, Target: target}
		in.Normalize()
		return in
	}
	with := func(in Input, f func(*Input)) Input { f(&in); return in }

	tests := []struct {
		name      string
		in        Input
		limits    auth.Limits
		wantField string
	}{
		{"http válido", base("http", "https://exemplo.com/health"), free, ""},
		{"http sem esquema ganha https", base("http", "exemplo.com"), free, ""},
		{"http com esquema estranho", base("http", "ftp://exemplo.com"), free, "target"},
		{"tcp válido", base("tcp", "db.exemplo.com:5432"), free, ""},
		{"tcp sem porta", base("tcp", "db.exemplo.com"), free, "target"},
		{"tcp porta inválida", base("tcp", "db.exemplo.com:99999"), free, "target"},
		{"ping válido", base("ping", "1.1.1.1"), free, ""},
		{"ping com URL", base("ping", "https://1.1.1.1"), free, "target"},
		{"ssl domínio", base("ssl", "exemplo.com"), free, ""},
		{"ssl com http", base("ssl", "http://exemplo.com"), free, "target"},
		{"heartbeat ignora alvo", base("heartbeat", "qualquer coisa"), free, ""},
		{"tipo inválido", base("smtp", "x"), free, "kind"},
		{"sem projeto", with(base("http", "https://x.com"), func(i *Input) { i.ProjectID = 0 }), free, "project_id"},
		{"sem nome", with(base("http", "https://x.com"), func(i *Input) { i.Name = "" }), free, "name"},
		{"intervalo abaixo do plano free", with(base("http", "https://x.com"), func(i *Input) { i.IntervalSeconds = 30 }), free, "interval_seconds"},
		{"intervalo de 30s ok no pro", with(base("http", "https://x.com"), func(i *Input) { i.IntervalSeconds = 30 }), pro, ""},
		{"timeout maior que intervalo", with(base("http", "https://x.com"), func(i *Input) { i.IntervalSeconds = 60; i.TimeoutSeconds = 60 }), free, "timeout_seconds"},
		{"retries demais", with(base("http", "https://x.com"), func(i *Input) { i.Retries = intp(9) }), free, "retries"},
		{"status esperado inválido", with(base("http", "https://x.com"), func(i *Input) { i.ExpectedStatus = 42 }), free, "expected_status"},
		{"heartbeat aceita tolerância longa", with(base("heartbeat", ""), func(i *Input) { i.IntervalSeconds = 3600; i.TimeoutSeconds = 1800 }), free, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.in.Validate(tt.limits)
			if tt.wantField == "" {
				if errs != nil {
					t.Fatalf("erros inesperados: %v", errs)
				}
				return
			}
			if _, ok := errs[tt.wantField]; !ok {
				t.Fatalf("queria erro em %q, veio %v", tt.wantField, errs)
			}
		})
	}
}

func TestNormalizeDefaults(t *testing.T) {
	tests := []struct {
		kind          string
		wantInterval  int
		wantTimeout   int
		wantThreshold int
	}{
		{"http", 60, 10, 3},
		{"ssl", 12 * 3600, 10, 1},
		{"heartbeat", 3600, 300, 1},
	}
	for _, tt := range tests {
		in := Input{Kind: tt.kind}
		in.Normalize()
		if in.IntervalSeconds != tt.wantInterval || in.TimeoutSeconds != tt.wantTimeout || in.FailureThreshold != tt.wantThreshold {
			t.Errorf("%s: %+v", tt.kind, in)
		}
		wantRetries := 1
		if tt.kind == "heartbeat" {
			wantRetries = 0
		}
		if in.Retries == nil || *in.Retries != wantRetries || in.Enabled == nil || !*in.Enabled {
			t.Errorf("%s: retries/enabled padrão errados", tt.kind)
		}
	}
}

func TestTargetOf(t *testing.T) {
	hb := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	m := store.Monitor{
		Kind: "heartbeat", IntervalSeconds: 3600, TimeoutSeconds: 300, Retries: 2,
		LastHeartbeatAt: &hb,
	}
	tg := TargetOf(m)
	if tg.Kind != checker.KindHeartbeat || tg.Interval != time.Hour || tg.Timeout != 5*time.Minute || tg.Retries != 2 || !tg.LastHeartbeat.Equal(hb) {
		t.Fatalf("target = %+v", tg)
	}
}
