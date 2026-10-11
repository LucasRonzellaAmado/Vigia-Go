package statuspage

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/luquinha/vigia/internal/incident"
	"github.com/luquinha/vigia/internal/store"
	"github.com/luquinha/vigia/internal/web"
)

//go:embed templates
var templatesFS embed.FS

const days = 90

type Store interface {
	GetProjectBySlug(ctx context.Context, slug string) (store.Project, error)
	GetProjectByDomain(ctx context.Context, customDomain *string) (store.Project, error)
	ListMonitorsByProject(ctx context.Context, projectID int64) ([]store.Monitor, error)
	ListIncidentsForProjectSince(ctx context.Context, arg store.ListIncidentsForProjectSinceParams) ([]store.ListIncidentsForProjectSinceRow, error)
}

type Handler struct {
	store    Store
	render   *web.Renderer
	loc      *time.Location
	baseHost string
	logger   *slog.Logger
	now      func() time.Time
}

func New(s Store, loc *time.Location, baseURL string, logger *slog.Logger) (*Handler, error) {
	sub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		return nil, err
	}
	r, err := web.NewRenderer(sub, "layout.html", []string{"status.html", "notfound.html"}, web.Funcs(loc), logger)
	if err != nil {
		return nil, err
	}
	return &Handler{store: s, render: r, loc: loc, baseHost: hostOf(baseURL), logger: logger, now: time.Now}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /status/{slug}", h.bySlug(false))
	mux.HandleFunc("GET /status/{slug}/live", h.bySlug(true))
}

func (h *Handler) CustomDomains(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostOf(r.Host)
		if host == "" || host == h.baseHost || host == "localhost" || net.ParseIP(host) != nil {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		project, err := h.store.GetProjectByDomain(r.Context(), &host)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/":
			h.serve(w, r, project, false, "/live")
		case "/live":
			h.serve(w, r, project, true, "/live")
		default:
			http.Redirect(w, r, "/", http.StatusFound)
		}
	})
}

func (h *Handler) bySlug(live bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project, err := h.store.GetProjectBySlug(r.Context(), r.PathValue("slug"))
		if err != nil {
			if !store.IsNotFound(err) {
				h.logger.Error("status page", "error", err)
			}
			h.render.Page(w, http.StatusNotFound, "notfound", nil)
			return
		}
		h.serve(w, r, project, live, "/status/"+project.Slug+"/live")
	}
}

type MonitorStatus struct {
	Name     string
	Status   string
	Enabled  bool
	Uptime   float64
	HasData  bool
	Bars     []incident.Day
	Since    *time.Time
	KindName string
}

type IncidentView struct {
	MonitorName string
	StartedAt   time.Time
	ResolvedAt  *time.Time
	Cause       string
}

type Page struct {
	Project   store.Project
	Overall   string
	Headline  string
	Monitors  []MonitorStatus
	Incidents []IncidentView
	Updated   time.Time
	LiveURL   string
	Days      int
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, project store.Project, live bool, liveURL string) {
	if !project.Public {
		h.render.Page(w, http.StatusNotFound, "notfound", nil)
		return
	}
	page, err := h.build(r.Context(), project)
	if err != nil {
		h.logger.Error("montar status page", "project_id", project.ID, "error", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}
	page.LiveURL = liveURL

	w.Header().Set("Cache-Control", "public, max-age=30")
	if live {
		h.render.Fragment(w, http.StatusOK, "status", "live", page)
		return
	}
	h.render.Page(w, http.StatusOK, "status", page)
}

func (h *Handler) build(ctx context.Context, project store.Project) (Page, error) {
	now := h.now()
	since := now.AddDate(0, 0, -days)

	monitors, err := h.store.ListMonitorsByProject(ctx, project.ID)
	if err != nil {
		return Page{}, err
	}
	incs, err := h.store.ListIncidentsForProjectSince(ctx, store.ListIncidentsForProjectSinceParams{ProjectID: project.ID, Since: since})
	if err != nil {
		return Page{}, err
	}

	byMonitor := make(map[int64][]incident.Interval)
	for _, inc := range incs {
		iv := incident.Interval{Start: inc.StartedAt}
		if inc.ResolvedAt != nil {
			iv.End = *inc.ResolvedAt
		}
		byMonitor[inc.MonitorID] = append(byMonitor[inc.MonitorID], iv)
	}

	page := Page{Project: project, Updated: now, Days: days}
	var active, down int
	for _, m := range monitors {
		if !m.Enabled {
			continue
		}
		active++
		ivs := byMonitor[m.ID]
		up, ok := incident.Uptime(ivs, since, now, m.CreatedAt)
		ms := MonitorStatus{
			Name:     m.Name,
			Status:   m.Status,
			Enabled:  m.Enabled,
			Uptime:   up,
			HasData:  ok,
			Bars:     incident.DailyBars(ivs, m.CreatedAt, now, days, h.loc),
			KindName: web.KindLabel(m.Kind),
		}
		if m.Status == "down" {
			down++
			ms.Since = m.FailingSince
		}
		page.Monitors = append(page.Monitors, ms)
	}

	switch {
	case active == 0:
		page.Overall, page.Headline = "ok", "Nenhum serviço monitorado ainda"
	case down == 0:
		page.Overall, page.Headline = "ok", "Todos os sistemas operacionais"
	case down == active:
		page.Overall, page.Headline = "major", "Todos os serviços estão fora do ar"
	default:
		page.Overall, page.Headline = "partial", "Instabilidade em parte dos serviços"
	}

	for i, inc := range incs {
		if i >= 20 {
			break
		}
		page.Incidents = append(page.Incidents, IncidentView{
			MonitorName: inc.MonitorName,
			StartedAt:   inc.StartedAt,
			ResolvedAt:  inc.ResolvedAt,
			Cause:       publicCause(inc.Cause),
		})
	}
	return page, nil
}

func publicCause(cause string) string {
	c := strings.ToLower(cause)
	switch {
	case strings.Contains(c, "timeout"):
		return "Tempo de resposta esgotado"
	case strings.Contains(c, "certificado"):
		return "Problema no certificado SSL"
	case strings.Contains(c, "heartbeat"):
		return "Tarefa agendada não reportou no prazo"
	case strings.Contains(c, "status"):
		return "Respostas com erro"
	case strings.Contains(c, "dns"):
		return "Falha de DNS"
	}
	return "Serviço indisponível"
}

func hostOf(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.SplitN(s, "/", 2)[0]
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	return strings.ToLower(s)
}
