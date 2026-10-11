package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/incident"
	"github.com/luquinha/vigia/internal/monitor"
	"github.com/luquinha/vigia/internal/store"
)

type ChannelTester interface {
	SendTest(ctx context.Context, ch store.AlertChannel) error
	Supports(kind string) bool
}

type Handler struct {
	db       *store.DB
	auth     *auth.Service
	monitors *monitor.Service
	alerts   ChannelTester
	render   *Renderer
	loc      *time.Location
	baseURL  string
	logger   *slog.Logger
}

type View struct {
	Title   string
	User    store.User
	Limits  auth.Limits
	Active  string
	Flash   string
	Error   string
	BaseURL string
	Data    any
}

var pages = []string{
	"login.html", "signup.html", "dashboard.html", "monitor_form.html", "monitor_detail.html",
	"incidents.html", "projects.html", "project_form.html", "channels.html", "account.html", "error.html",
}

func NewHandler(db *store.DB, authSvc *auth.Service, monitors *monitor.Service, alerts ChannelTester, loc *time.Location, baseURL string, logger *slog.Logger) (*Handler, error) {
	r, err := NewRenderer(TemplatesFS(), "layout.html", pages, Funcs(loc), logger)
	if err != nil {
		return nil, err
	}
	return &Handler{db: db, auth: authSvc, monitors: monitors, alerts: alerts, render: r, loc: loc, baseURL: baseURL, logger: logger}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /static/", http.StripPrefix("/static/", StaticHandler()))

	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("POST /login", h.login)
	mux.HandleFunc("GET /signup", h.signupPage)
	mux.HandleFunc("POST /signup", h.signup)
	mux.HandleFunc("POST /logout", h.logout)

	p := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, auth.RequireUser(fn)) }
	p("GET /{$}", h.dashboard)
	p("GET /fragments/monitors", h.monitorsFragment)
	p("GET /monitors/new", h.newMonitor)
	p("GET /monitors/fields", h.monitorFields)
	p("POST /monitors", h.createMonitor)
	p("GET /monitors/{id}", h.showMonitor)
	p("GET /monitors/{id}/results", h.monitorResults)
	p("GET /monitors/{id}/edit", h.editMonitor)
	p("POST /monitors/{id}", h.updateMonitor)
	p("POST /monitors/{id}/toggle", h.toggleMonitor)
	p("POST /monitors/{id}/delete", h.deleteMonitor)
	p("GET /incidents", h.incidents)
	p("GET /projects", h.projects)
	p("POST /projects", h.createProject)
	p("GET /projects/{id}/edit", h.editProject)
	p("POST /projects/{id}", h.updateProject)
	p("POST /projects/{id}/delete", h.deleteProject)
	p("GET /channels", h.channels)
	p("POST /channels", h.createChannel)
	p("POST /channels/{id}/test", h.testChannel)
	p("POST /channels/{id}/delete", h.deleteChannel)
	p("GET /account", h.account)
}

func (h *Handler) view(r *http.Request, title, active string, data any) View {
	user, _ := auth.UserFrom(r.Context())
	return View{
		Title:   title,
		User:    user,
		Limits:  auth.LimitsFor(user.Plan),
		Active:  active,
		Flash:   r.URL.Query().Get("ok"),
		BaseURL: h.baseURL,
		Data:    data,
	}
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.Error("erro no painel", "path", r.URL.Path, "error", err)
	v := h.view(r, "Erro", "", nil)
	v.Error = "Algo deu errado do nosso lado. Tente de novo em instantes."
	h.render.Page(w, http.StatusInternalServerError, "error", v)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Não encontrado", "", nil)
	v.Error = "Não achei o que você procurava."
	h.render.Page(w, http.StatusNotFound, "error", v)
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

type authForm struct {
	Email       string
	Error       string
	AllowSignup bool
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserFrom(r.Context()); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render.Page(w, http.StatusOK, "login", h.view(r, "Entrar", "", authForm{AllowSignup: h.auth.SignupAllowed()}))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	email, password := r.PostFormValue("email"), r.PostFormValue("password")
	user, err := h.auth.Authenticate(r.Context(), email, password)
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			h.serverError(w, r, err)
			return
		}
		form := authForm{Email: email, Error: err.Error(), AllowSignup: h.auth.SignupAllowed()}
		h.render.Page(w, http.StatusUnauthorized, "login", h.view(r, "Entrar", "", form))
		return
	}
	if err := h.auth.StartSession(r.Context(), w, user.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) signupPage(w http.ResponseWriter, r *http.Request) {
	if !h.auth.SignupAllowed() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.render.Page(w, http.StatusOK, "signup", h.view(r, "Criar conta", "", authForm{AllowSignup: true}))
}

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	email, password := r.PostFormValue("email"), r.PostFormValue("password")
	user, err := h.auth.Signup(r.Context(), email, password)
	if err != nil {
		var verr auth.ValidationError
		if errors.As(err, &verr) || errors.Is(err, auth.ErrEmailTaken) || errors.Is(err, auth.ErrSignupClosed) {
			form := authForm{Email: email, Error: err.Error(), AllowSignup: h.auth.SignupAllowed()}
			h.render.Page(w, http.StatusUnprocessableEntity, "signup", h.view(r, "Criar conta", "", form))
			return
		}
		h.serverError(w, r, err)
		return
	}
	if err := h.auth.StartSession(r.Context(), w, user.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/?ok=Conta+criada!+Bora+cadastrar+o+primeiro+monitor.", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.EndSession(r.Context(), w, r); err != nil {
		h.logger.Warn("logout", "error", err)
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type monitorRow struct {
	store.ListMonitorsByUserRow
	Uptime24h float64
	HasUptime bool
}

type dashboardData struct {
	Monitors []monitorRow
	Up       int
	Down     int
	Paused   int
	Projects []store.Project
}

func (h *Handler) loadDashboard(r *http.Request) (dashboardData, error) {
	user, _ := auth.UserFrom(r.Context())
	rows, err := h.monitors.List(r.Context(), user)
	if err != nil {
		return dashboardData{}, err
	}
	projects, err := h.db.ListProjectsByUser(r.Context(), user.ID)
	if err != nil {
		return dashboardData{}, err
	}

	now := time.Now()
	since := now.Add(-24 * time.Hour)
	data := dashboardData{Projects: projects}
	byProject := map[int64][]store.ListIncidentsForProjectSinceRow{}
	for _, p := range projects {
		incs, err := h.db.ListIncidentsForProjectSince(r.Context(), store.ListIncidentsForProjectSinceParams{ProjectID: p.ID, Since: since})
		if err != nil {
			return dashboardData{}, err
		}
		byProject[p.ID] = incs
	}

	for _, m := range rows {
		var ivs []incident.Interval
		for _, inc := range byProject[m.ProjectID] {
			if inc.MonitorID == m.ID {
				iv := incident.Interval{Start: inc.StartedAt}
				if inc.ResolvedAt != nil {
					iv.End = *inc.ResolvedAt
				}
				ivs = append(ivs, iv)
			}
		}
		up, ok := incident.Uptime(ivs, since, now, m.CreatedAt)
		data.Monitors = append(data.Monitors, monitorRow{ListMonitorsByUserRow: m, Uptime24h: up, HasUptime: ok})

		switch {
		case !m.Enabled:
			data.Paused++
		case m.Status == "down":
			data.Down++
		case m.Status == "up":
			data.Up++
		}
	}
	return data, nil
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	data, err := h.loadDashboard(r)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render.Page(w, http.StatusOK, "dashboard", h.view(r, "Monitores", "monitors", data))
}

func (h *Handler) monitorsFragment(w http.ResponseWriter, r *http.Request) {
	data, err := h.loadDashboard(r)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render.Fragment(w, http.StatusOK, "dashboard", "monitor-table", h.view(r, "", "", data))
}

type monitorFormData struct {
	Monitor  *store.Monitor
	Input    monitor.Input
	Errors   monitor.ValidationErrors
	Projects []store.Project
	Kinds    []string
	Message  string
}

var kinds = []string{"http", "tcp", "ping", "ssl", "heartbeat"}

func (h *Handler) renderMonitorForm(w http.ResponseWriter, r *http.Request, status int, data monitorFormData) {
	user, _ := auth.UserFrom(r.Context())
	projects, err := h.db.ListProjectsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	data.Projects = projects
	data.Kinds = kinds
	title := "Novo monitor"
	if data.Monitor != nil {
		title = "Editar " + data.Monitor.Name
	}
	h.render.Page(w, status, "monitor_form", h.view(r, title, "monitors", data))
}

func (h *Handler) newMonitor(w http.ResponseWriter, r *http.Request) {
	projectID, _ := strconv.ParseInt(r.URL.Query().Get("project_id"), 10, 64)
	kind := r.URL.Query().Get("kind")
	if !slices.Contains(kinds, kind) {
		kind = "http"
	}
	h.renderMonitorForm(w, r, http.StatusOK, monitorFormData{Input: monitor.DefaultInput(kind, projectID)})
}

func (h *Handler) monitorFields(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if !slices.Contains(kinds, kind) {
		kind = "http"
	}
	data := monitorFormData{Input: monitor.DefaultInput(kind, 0)}
	h.render.Fragment(w, http.StatusOK, "monitor_form", "kind-fields", h.view(r, "", "", data))
}

func (h *Handler) createMonitor(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	in := monitor.InputFromForm(r)
	m, err := h.monitors.Create(r.Context(), user, in)
	if err != nil {
		h.monitorFormError(w, r, nil, in, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/monitors/%d?ok=Monitor+criado", m.ID))
}

func (h *Handler) monitorFormError(w http.ResponseWriter, r *http.Request, m *store.Monitor, in monitor.Input, err error) {
	var verrs monitor.ValidationErrors
	switch {
	case errors.As(err, &verrs):
		in.Normalize()
		h.renderMonitorForm(w, r, http.StatusUnprocessableEntity, monitorFormData{Monitor: m, Input: in, Errors: verrs})
	case errors.Is(err, monitor.ErrLimitReached):
		in.Normalize()
		h.renderMonitorForm(w, r, http.StatusForbidden, monitorFormData{Monitor: m, Input: in, Message: err.Error()})
	case errors.Is(err, monitor.ErrNotFound):
		h.notFound(w, r)
	default:
		h.serverError(w, r, err)
	}
}

type monitorDetail struct {
	Monitor      store.Monitor
	Project      store.Project
	Uptime       []uptimeWindow
	Latency      store.LatencyStatsRow
	Bars         []incident.Day
	Results      []store.CheckResult
	Incidents    []store.Incident
	HeartbeatURL string
}

type uptimeWindow struct {
	Label string
	Value float64
	OK    bool
}

func (h *Handler) showMonitor(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	m, err := h.monitors.Get(r.Context(), user, id)
	if err != nil {
		h.monitorFormError(w, r, nil, monitor.Input{}, err)
		return
	}
	ctx := r.Context()
	project, err := h.db.GetProjectForUser(ctx, store.GetProjectForUserParams{ID: m.ProjectID, UserID: user.ID})
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	now := time.Now()
	incs, err := h.db.ListIncidentsForMonitorSince(ctx, store.ListIncidentsForMonitorSinceParams{MonitorID: m.ID, Since: now.AddDate(0, 0, -90)})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	latency, err := h.db.LatencyStats(ctx, store.LatencyStatsParams{MonitorID: m.ID, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	results, err := h.monitors.Results(ctx, user, m.ID, 20)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	ivs := incident.IntervalsOf(incs)
	var windows []uptimeWindow
	for _, wdw := range []struct {
		label string
		d     time.Duration
	}{{"24h", 24 * time.Hour}, {"7 dias", 7 * 24 * time.Hour}, {"30 dias", 30 * 24 * time.Hour}, {"90 dias", 90 * 24 * time.Hour}} {
		v, ok := incident.Uptime(ivs, now.Add(-wdw.d), now, m.CreatedAt)
		windows = append(windows, uptimeWindow{Label: wdw.label, Value: v, OK: ok})
	}

	slices.Reverse(incs)
	detail := monitorDetail{
		Monitor:   m,
		Project:   project,
		Uptime:    windows,
		Latency:   latency,
		Bars:      incident.DailyBars(ivs, m.CreatedAt, now, 90, h.loc),
		Results:   results,
		Incidents: incs,
	}
	if m.HeartbeatToken != nil {
		detail.HeartbeatURL = h.baseURL + "/hb/" + *m.HeartbeatToken
	}
	h.render.Page(w, http.StatusOK, "monitor_detail", h.view(r, m.Name, "monitors", detail))
}

func (h *Handler) monitorResults(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	m, err := h.monitors.Get(r.Context(), user, id)
	if err != nil {
		h.monitorFormError(w, r, nil, monitor.Input{}, err)
		return
	}
	results, err := h.monitors.Results(r.Context(), user, id, 20)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render.Fragment(w, http.StatusOK, "monitor_detail", "results", h.view(r, "", "", monitorDetail{Monitor: m, Results: results}))
}

func (h *Handler) editMonitor(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	m, err := h.monitors.Get(r.Context(), user, id)
	if err != nil {
		h.monitorFormError(w, r, nil, monitor.Input{}, err)
		return
	}
	h.renderMonitorForm(w, r, http.StatusOK, monitorFormData{Monitor: &m, Input: monitor.InputFromMonitor(m)})
}

func (h *Handler) updateMonitor(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	current, err := h.monitors.Get(r.Context(), user, id)
	if err != nil {
		h.monitorFormError(w, r, nil, monitor.Input{}, err)
		return
	}
	in := monitor.InputFromForm(r)
	in.Kind = current.Kind
	if _, err := h.monitors.Update(r.Context(), user, id, in); err != nil {
		h.monitorFormError(w, r, &current, in, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/monitors/%d?ok=Monitor+atualizado", id))
}

func (h *Handler) toggleMonitor(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	m, err := h.monitors.Get(r.Context(), user, id)
	if err != nil {
		h.monitorFormError(w, r, nil, monitor.Input{}, err)
		return
	}
	if _, err := h.monitors.SetEnabled(r.Context(), user, id, !m.Enabled); err != nil {
		h.serverError(w, r, err)
		return
	}
	msg := "Monitor+pausado"
	if !m.Enabled {
		msg = "Monitor+reativado"
	}
	redirect(w, r, fmt.Sprintf("/monitors/%d?ok=%s", id, msg))
}

func (h *Handler) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	if err := h.monitors.Delete(r.Context(), user, id); err != nil {
		h.monitorFormError(w, r, nil, monitor.Input{}, err)
		return
	}
	redirect(w, r, "/?ok=Monitor+apagado")
}

func (h *Handler) incidents(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	rows, err := h.db.ListIncidentsByUser(r.Context(), store.ListIncidentsByUserParams{UserID: user.ID, MaxRows: 200})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render.Page(w, http.StatusOK, "incidents", h.view(r, "Incidentes", "incidents", rows))
}

type projectsData struct {
	Projects []store.Project
	Form     projectForm
}

type projectForm struct {
	ID           int64
	Name         string
	Slug         string
	CustomDomain string
	Public       bool
	Error        string
}

func (h *Handler) renderProjects(w http.ResponseWriter, r *http.Request, status int, form projectForm) {
	user, _ := auth.UserFrom(r.Context())
	projects, err := h.db.ListProjectsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render.Page(w, status, "projects", h.view(r, "Projetos", "projects", projectsData{Projects: projects, Form: form}))
}

func (h *Handler) projects(w http.ResponseWriter, r *http.Request) {
	h.renderProjects(w, r, http.StatusOK, projectForm{Public: true})
}

func projectFormFrom(r *http.Request) projectForm {
	return projectForm{
		Name:         strings.TrimSpace(r.PostFormValue("name")),
		Slug:         strings.TrimSpace(r.PostFormValue("slug")),
		CustomDomain: strings.ToLower(strings.TrimSpace(r.PostFormValue("custom_domain"))),
		Public:       r.PostFormValue("public") != "",
	}
}

func (h *Handler) validateProject(r *http.Request, f *projectForm) (*string, bool) {
	user, _ := auth.UserFrom(r.Context())
	limits := auth.LimitsFor(user.Plan)
	if f.Name == "" || len(f.Name) > 80 {
		f.Error = "Dê um nome ao projeto (até 80 caracteres)."
		return nil, false
	}
	if f.Slug != "" {
		f.Slug = auth.Slugify(f.Slug)
		if f.Slug == "" {
			f.Error = "Slug inválido: use letras, números e hífens."
			return nil, false
		}
	}
	if f.CustomDomain == "" {
		return nil, true
	}
	if !limits.CustomDomain {
		f.Error = "Domínio próprio na página de status é recurso do plano Pro."
		return nil, false
	}
	if strings.ContainsAny(f.CustomDomain, "/: ") || !strings.Contains(f.CustomDomain, ".") {
		f.Error = "Domínio inválido, use algo como status.meusite.com"
		return nil, false
	}
	d := f.CustomDomain
	return &d, true
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	form := projectFormFrom(r)
	domain, ok := h.validateProject(r, &form)
	if !ok {
		h.renderProjects(w, r, http.StatusUnprocessableEntity, form)
		return
	}
	count, err := h.db.CountProjectsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if limits := auth.LimitsFor(user.Plan); int(count) >= limits.MaxProjects {
		form.Error = fmt.Sprintf("Seu plano %s permite %d projetos.", limits.Plan.Label(), limits.MaxProjects)
		h.renderProjects(w, r, http.StatusForbidden, form)
		return
	}
	if domain != nil {
		if _, err := h.db.GetProjectByDomain(r.Context(), domain); err == nil {
			form.Error = "Esse domínio já está em uso."
			h.renderProjects(w, r, http.StatusConflict, form)
			return
		}
	}
	slugBase := form.Slug
	if slugBase == "" {
		slugBase = form.Name
	}
	if _, err := auth.CreateProjectWithUniqueSlug(r.Context(), h.db.Queries, user.ID, form.Name, slugBase, domain, form.Public); err != nil {
		h.serverError(w, r, err)
		return
	}
	redirect(w, r, "/projects?ok=Projeto+criado")
}

func (h *Handler) editProject(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	p, err := h.db.GetProjectForUser(r.Context(), store.GetProjectForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		if store.IsNotFound(err) {
			h.notFound(w, r)
			return
		}
		h.serverError(w, r, err)
		return
	}
	form := projectForm{ID: p.ID, Name: p.Name, Slug: p.Slug, CustomDomain: derefStr(p.CustomDomain), Public: p.Public}
	h.render.Page(w, http.StatusOK, "project_form", h.view(r, "Editar projeto", "projects", form))
}

func (h *Handler) updateProject(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	form := projectFormFrom(r)
	form.ID = id
	domain, ok := h.validateProject(r, &form)
	if ok && form.Slug == "" {
		form.Error = "O slug é obrigatório."
		ok = false
	}
	if !ok {
		h.render.Page(w, http.StatusUnprocessableEntity, "project_form", h.view(r, "Editar projeto", "projects", form))
		return
	}
	_, err := h.db.UpdateProject(r.Context(), store.UpdateProjectParams{
		ID: id, UserID: user.ID, Name: form.Name, Slug: form.Slug, CustomDomain: domain, Public: form.Public,
	})
	switch {
	case store.IsUniqueViolation(err):
		form.Error = "Slug ou domínio já em uso por outro projeto."
		h.render.Page(w, http.StatusConflict, "project_form", h.view(r, "Editar projeto", "projects", form))
		return
	case store.IsNotFound(err):
		h.notFound(w, r)
		return
	case err != nil:
		h.serverError(w, r, err)
		return
	}
	redirect(w, r, "/projects?ok=Projeto+atualizado")
}

func (h *Handler) deleteProject(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	count, err := h.db.CountProjectsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if count <= 1 {
		redirect(w, r, "/projects?ok=Você+precisa+de+pelo+menos+um+projeto")
		return
	}
	if _, err := h.db.DeleteProjectForUser(r.Context(), store.DeleteProjectForUserParams{ID: id, UserID: user.ID}); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.monitors.Changed()
	redirect(w, r, "/projects?ok=Projeto+apagado")
}

type channelsData struct {
	Channels []store.ListAlertChannelsByUserRow
	Projects []store.Project
	Kinds    []channelKind
	Form     channelForm
}

type channelKind struct {
	Value       string
	Label       string
	Placeholder string
	Available   bool
}

type channelForm struct {
	ProjectID int64
	Kind      string
	Name      string
	Target    string
	Error     string
}

func (h *Handler) channelKinds() []channelKind {
	return []channelKind{
		{"telegram", "Telegram", "chat_id, ex.: 123456789", h.alerts.Supports("telegram")},
		{"email", "E-mail", "voce@exemplo.com", h.alerts.Supports("email")},
		{"webhook", "Webhook", "https://exemplo.com/hooks/vigia", h.alerts.Supports("webhook")},
		{"whatsapp", "WhatsApp", "5519999999999", h.alerts.Supports("whatsapp")},
	}
}

func (h *Handler) renderChannels(w http.ResponseWriter, r *http.Request, status int, form channelForm) {
	user, _ := auth.UserFrom(r.Context())
	chans, err := h.db.ListAlertChannelsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	projects, err := h.db.ListProjectsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	data := channelsData{Channels: chans, Projects: projects, Kinds: h.channelKinds(), Form: form}
	h.render.Page(w, status, "channels", h.view(r, "Canais de alerta", "channels", data))
}

func (h *Handler) channels(w http.ResponseWriter, r *http.Request) {
	h.renderChannels(w, r, http.StatusOK, channelForm{Kind: "telegram"})
}

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	projectID, _ := strconv.ParseInt(r.PostFormValue("project_id"), 10, 64)
	form := channelForm{
		ProjectID: projectID,
		Kind:      r.PostFormValue("kind"),
		Name:      strings.TrimSpace(r.PostFormValue("name")),
		Target:    strings.TrimSpace(r.PostFormValue("target")),
	}

	if msg := validateChannel(form); msg != "" {
		form.Error = msg
		h.renderChannels(w, r, http.StatusUnprocessableEntity, form)
		return
	}
	if _, err := h.db.GetProjectForUser(r.Context(), store.GetProjectForUserParams{ID: projectID, UserID: user.ID}); err != nil {
		form.Error = "Projeto inválido."
		h.renderChannels(w, r, http.StatusUnprocessableEntity, form)
		return
	}
	existing, err := h.db.ListAlertChannelsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if limits := auth.LimitsFor(user.Plan); len(existing) >= limits.MaxChannels {
		form.Error = fmt.Sprintf("Seu plano %s permite %d canais.", limits.Plan.Label(), limits.MaxChannels)
		h.renderChannels(w, r, http.StatusForbidden, form)
		return
	}
	if _, err := h.db.CreateAlertChannel(r.Context(), store.CreateAlertChannelParams{
		ProjectID: projectID, Kind: form.Kind, Name: form.Name, Target: form.Target, Enabled: true,
	}); err != nil {
		h.serverError(w, r, err)
		return
	}
	redirect(w, r, "/channels?ok=Canal+criado.+Use+o+botão+Testar+para+conferir.")
}

func validateChannel(f channelForm) string {
	if f.Name == "" || len(f.Name) > 80 {
		return "Dê um nome ao canal."
	}
	if f.Target == "" || len(f.Target) > 500 {
		return "Informe o destino do alerta."
	}
	switch f.Kind {
	case "telegram":
		if _, err := strconv.ParseInt(f.Target, 10, 64); err != nil && !strings.HasPrefix(f.Target, "@") {
			return "No Telegram use o chat_id numérico (ou @canal)."
		}
	case "email":
		if _, err := auth.NormalizeEmail(f.Target); err != nil {
			return "E-mail inválido."
		}
	case "webhook":
		if !strings.HasPrefix(f.Target, "https://") && !strings.HasPrefix(f.Target, "http://") {
			return "O webhook precisa ser uma URL http(s)."
		}
	case "whatsapp":
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, f.Target)
		if len(digits) < 10 {
			return "Informe o telefone com DDI e DDD, ex.: 5519999999999."
		}
	default:
		return "Tipo de canal inválido."
	}
	return ""
}

func (h *Handler) testChannel(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	ch, err := h.db.GetAlertChannelForUser(r.Context(), store.GetAlertChannelForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		h.notFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result := map[string]any{"OK": true, "Message": "Enviado! Confere aí."}
	if err := h.alerts.SendTest(ctx, ch); err != nil {
		result = map[string]any{"OK": false, "Message": err.Error()}
	}
	h.render.Fragment(w, http.StatusOK, "channels", "test-result", result)
}

func (h *Handler) deleteChannel(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := idParam(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	if _, err := h.db.DeleteAlertChannelForUser(r.Context(), store.DeleteAlertChannelForUserParams{ID: id, UserID: user.ID}); err != nil {
		h.serverError(w, r, err)
		return
	}
	redirect(w, r, "/channels?ok=Canal+apagado")
}

type accountData struct {
	Free         auth.Limits
	Pro          auth.Limits
	MonitorCount int64
	ProjectCount int64
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	mc, err := h.db.CountMonitorsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	pc, err := h.db.CountProjectsByUser(r.Context(), user.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	data := accountData{Free: auth.LimitsFor("free"), Pro: auth.LimitsFor("pro"), MonitorCount: mc, ProjectCount: pc}
	h.render.Page(w, http.StatusOK, "account", h.view(r, "Conta e plano", "account", data))
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
