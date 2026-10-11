package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/incident"
	"github.com/luquinha/vigia/internal/monitor"
	"github.com/luquinha/vigia/internal/store"
)

func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	r, err := NewRenderer(TemplatesFS(), "layout.html", pages, Funcs(loc), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("templates não compilam: %v", err)
	}
	return r
}

func TestPagesRender(t *testing.T) {
	r := newTestRenderer(t)
	now := time.Now().UTC()
	user := store.User{ID: 1, Email: "lucas@exemplo.com", Plan: "free", CreatedAt: now}
	view := func(data any) View {
		return View{Title: "t", User: user, Limits: auth.LimitsFor("free"), Data: data, BaseURL: "http://x"}
	}
	token := "abcdefghijklmnopqrstuvwx"
	resolved := now.Add(-time.Hour)
	mon := store.Monitor{ID: 7, Name: "API", Kind: "heartbeat", Status: "down", Enabled: true, IntervalSeconds: 3600, HeartbeatToken: &token, CreatedAt: now.Add(-48 * time.Hour)}

	tests := []struct {
		page string
		data any
		want string
	}{
		{"login", authForm{Email: "a@b.c", Error: "e-mail ou senha incorretos", AllowSignup: true}, "senha incorretos"},
		{"signup", authForm{AllowSignup: true}, "Criar conta"},
		{"dashboard", dashboardData{Monitors: []monitorRow{{ListMonitorsByUserRow: store.ListMonitorsByUserRow{ID: 1, Name: "Site", Kind: "http", Target: "https://x.com", Status: "up", Enabled: true, ProjectName: "P"}, Uptime24h: 0.9995, HasUptime: true}}, Up: 1}, "99,95%"},
		{"dashboard", dashboardData{}, "Nada sendo vigiado"},
		{"monitor_form", monitorFormData{Input: monitor.DefaultInput("http", 1), Kinds: kinds, Errors: monitor.ValidationErrors{"target": "URL ruim"}}, "URL ruim"},
		{"monitor_form", monitorFormData{Monitor: &mon, Input: monitor.InputFromMonitor(mon)}, "Tolerância de atraso"},
		{"monitor_detail", monitorDetail{
			Monitor: mon, Project: store.Project{Name: "P"},
			Uptime:       []uptimeWindow{{"24h", 1, true}},
			Bars:         incident.DailyBars(nil, mon.CreatedAt, now, 90, time.UTC),
			Results:      []store.CheckResult{{Ok: false, Message: "sem heartbeat há 2h", CheckedAt: now}},
			Incidents:    []store.Incident{{StartedAt: now.Add(-3 * time.Hour), ResolvedAt: &resolved, Cause: "timeout"}, {StartedAt: now.Add(-time.Minute), Cause: "x"}},
			HeartbeatURL: "http://x/hb/" + token,
		}, "/hb/" + token},
		{"incidents", []store.ListIncidentsByUserRow{{MonitorName: "API", StartedAt: now}}, "em aberto"},
		{"projects", projectsData{Projects: []store.Project{{ID: 1, Name: "P", Slug: "p", Public: true}}}, "/status/p"},
		{"project_form", projectForm{ID: 1, Name: "P", Slug: "p"}, "Editar projeto"},
		{"channels", channelsData{Channels: []store.ListAlertChannelsByUserRow{{ID: 1, Name: "TG", Kind: "telegram", Target: "1"}}, Kinds: []channelKind{{"telegram", "Telegram", "", true}}}, "Testar"},
		{"account", accountData{Free: auth.LimitsFor("free"), Pro: auth.LimitsFor("pro")}, "set-plan"},
		{"error", nil, "Ops"},
	}
	for _, tt := range tests {
		t.Run(tt.page, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.Page(w, http.StatusOK, tt.page, view(tt.data))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.want) {
				t.Errorf("página %s sem %q", tt.page, tt.want)
			}
		})
	}
}

func TestFragmentsRender(t *testing.T) {
	r := newTestRenderer(t)
	tests := []struct {
		page, block string
		data        any
		want        string
	}{
		{"dashboard", "monitor-table", View{Data: dashboardData{}}, `id="monitor-table"`},
		{"monitor_form", "kind-fields", View{Data: monitorFormData{Input: monitor.DefaultInput("ssl", 0)}}, "ssl_expiry_days"},
		{"monitor_detail", "results", View{Data: monitorDetail{Monitor: store.Monitor{ID: 3}}}, `/monitors/3/results`},
		{"channels", "test-result", map[string]any{"OK": false, "Message": "chat not found"}, "chat not found"},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		r.Fragment(w, http.StatusOK, tt.page, tt.block, tt.data)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tt.want) {
			t.Errorf("%s/%s: %d %s", tt.page, tt.block, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "<html") {
			t.Errorf("%s/%s: fragmento não deveria trazer o layout", tt.page, tt.block)
		}
	}
}

func TestPct(t *testing.T) {
	pct := Funcs(time.UTC)["pct"].(func(float64) string)
	tests := []struct {
		in   float64
		want string
	}{
		{1, "100%"},
		{0.99999, "99,99%"},
		{0.5, "50,00%"},
		{0, "0,00%"},
	}
	for _, tt := range tests {
		if got := pct(tt.in); got != tt.want {
			t.Errorf("pct(%v) = %q, queria %q", tt.in, got, tt.want)
		}
	}
}

func TestValidateChannel(t *testing.T) {
	tests := []struct {
		form channelForm
		ok   bool
	}{
		{channelForm{Kind: "telegram", Name: "tg", Target: "123456"}, true},
		{channelForm{Kind: "telegram", Name: "tg", Target: "-1001234"}, true},
		{channelForm{Kind: "telegram", Name: "tg", Target: "fulano"}, false},
		{channelForm{Kind: "email", Name: "e", Target: "eu@exemplo.com"}, true},
		{channelForm{Kind: "email", Name: "e", Target: "eu"}, false},
		{channelForm{Kind: "webhook", Name: "w", Target: "https://hook"}, true},
		{channelForm{Kind: "webhook", Name: "w", Target: "hook"}, false},
		{channelForm{Kind: "whatsapp", Name: "z", Target: "+55 (19) 99999-9999"}, true},
		{channelForm{Kind: "whatsapp", Name: "z", Target: "123"}, false},
		{channelForm{Kind: "pombo", Name: "p", Target: "x"}, false},
		{channelForm{Kind: "telegram", Name: "", Target: "1"}, false},
	}
	for _, tt := range tests {
		if got := validateChannel(tt.form) == ""; got != tt.ok {
			t.Errorf("%+v: ok=%v", tt.form, got)
		}
	}
}
