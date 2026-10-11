package statuspage

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luquinha/vigia/internal/store"
)

type fakeStore struct {
	projects  []store.Project
	monitors  map[int64][]store.Monitor
	incidents map[int64][]store.ListIncidentsForProjectSinceRow
}

func (f *fakeStore) GetProjectBySlug(ctx context.Context, slug string) (store.Project, error) {
	for _, p := range f.projects {
		if p.Slug == slug {
			return p, nil
		}
	}
	return store.Project{}, pgx.ErrNoRows
}

func (f *fakeStore) GetProjectByDomain(ctx context.Context, domain *string) (store.Project, error) {
	for _, p := range f.projects {
		if p.CustomDomain != nil && *p.CustomDomain == *domain {
			return p, nil
		}
	}
	return store.Project{}, pgx.ErrNoRows
}

func (f *fakeStore) ListMonitorsByProject(ctx context.Context, projectID int64) ([]store.Monitor, error) {
	return f.monitors[projectID], nil
}

func (f *fakeStore) ListIncidentsForProjectSince(ctx context.Context, arg store.ListIncidentsForProjectSinceParams) ([]store.ListIncidentsForProjectSinceRow, error) {
	return f.incidents[arg.ProjectID], nil
}

func newTestHandler(t *testing.T) (*Handler, http.Handler) {
	t.Helper()
	now := time.Now().UTC()
	domain := "status.loja.com"
	f := &fakeStore{
		projects: []store.Project{
			{ID: 1, Name: "Loja", Slug: "loja", Public: true, CustomDomain: &domain},
			{ID: 2, Name: "Interno", Slug: "interno", Public: false},
			{ID: 3, Name: "Tudo fora", Slug: "fora", Public: true},
		},
		monitors: map[int64][]store.Monitor{
			1: {
				{ID: 1, Name: "Site", Kind: "http", Target: "https://segredo.interno", Status: "up", Enabled: true, CreatedAt: now.AddDate(0, 0, -100)},
				{ID: 2, Name: "Pagamentos", Kind: "http", Status: "down", Enabled: true, CreatedAt: now.AddDate(0, 0, -100), FailingSince: &now},
				{ID: 3, Name: "Pausado", Kind: "tcp", Status: "unknown", Enabled: false, CreatedAt: now},
			},
			3: {{ID: 4, Name: "API", Kind: "http", Status: "down", Enabled: true, CreatedAt: now.AddDate(0, 0, -1)}},
		},
		incidents: map[int64][]store.ListIncidentsForProjectSinceRow{
			1: {{MonitorID: 2, MonitorName: "Pagamentos", StartedAt: now.Add(-time.Hour), Cause: "timeout após 10s"}},
		},
	}
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	h, err := New(f, loc, "https://vigia.exemplo.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })
	return h, h.CustomDomains(fallback)
}

func TestStatusPage(t *testing.T) {
	_, handler := newTestHandler(t)

	tests := []struct {
		name       string
		host, path string
		wantStatus int
		want       []string
		notWant    []string
	}{
		{
			name: "página parcial", host: "vigia.exemplo.com", path: "/status/loja", wantStatus: 200,
			want:    []string{"Instabilidade em parte dos serviços", "Pagamentos", "Tempo de resposta esgotado", "Operacional"},
			notWant: []string{"segredo.interno", "Pausado"},
		},
		{name: "fragmento ao vivo", host: "vigia.exemplo.com", path: "/status/loja/live", wantStatus: 200, want: []string{`id="live"`}, notWant: []string{"<html"}},
		{name: "projeto privado", host: "vigia.exemplo.com", path: "/status/interno", wantStatus: 404},
		{name: "inexistente", host: "vigia.exemplo.com", path: "/status/nada", wantStatus: 404},
		{name: "tudo fora", host: "vigia.exemplo.com", path: "/status/fora", wantStatus: 200, want: []string{"Todos os serviços estão fora do ar"}},
		{name: "domínio próprio", host: "status.loja.com", path: "/", wantStatus: 200, want: []string{"Loja", `hx-get="/live"`}},
		{name: "domínio próprio redireciona outros caminhos", host: "status.loja.com", path: "/login", wantStatus: 302},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			r.Host = tt.host
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, queria %d", w.Code, tt.wantStatus)
			}
			body := w.Body.String()
			for _, s := range tt.want {
				if !strings.Contains(body, s) {
					t.Errorf("faltou %q", s)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(body, s) {
					t.Errorf("não deveria conter %q", s)
				}
			}
		})
	}
}

func TestPublicCause(t *testing.T) {
	tests := []struct{ in, want string }{
		{"timeout após 10s", "Tempo de resposta esgotado"},
		{"certificado expira em 3 dias", "Problema no certificado SSL"},
		{"status 502 (esperado 2xx/3xx)", "Respostas com erro"},
		{"conexão falhou: connect: connection refused 10.0.0.5:5432", "Serviço indisponível"},
	}
	for _, tt := range tests {
		if got := publicCause(tt.in); got != tt.want {
			t.Errorf("publicCause(%q) = %q", tt.in, got)
		}
	}
}

func TestHostOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://Vigia.Exemplo.com/", "vigia.exemplo.com"},
		{"localhost:8080", "localhost"},
		{"status.loja.com", "status.loja.com"},
	}
	for _, tt := range tests {
		if got := hostOf(tt.in); got != tt.want {
			t.Errorf("hostOf(%q) = %q", tt.in, got)
		}
	}
}
