package monitor

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/store"
)

type API struct {
	svc    *Service
	db     *store.DB
	logger *slog.Logger
}

func NewAPI(svc *Service, db *store.DB, logger *slog.Logger) *API {
	return &API{svc: svc, db: db, logger: logger}
}

func (a *API) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, wrap(fn)) }
	h("GET /api/v1/projects", a.listProjects)
	h("GET /api/v1/monitors", a.list)
	h("POST /api/v1/monitors", a.create)
	h("GET /api/v1/monitors/{id}", a.get)
	h("PUT /api/v1/monitors/{id}", a.update)
	h("DELETE /api/v1/monitors/{id}", a.delete)
	h("POST /api/v1/monitors/{id}/pause", a.setEnabled(false))
	h("POST /api/v1/monitors/{id}/resume", a.setEnabled(true))
	h("GET /api/v1/monitors/{id}/results", a.results)
	h("GET /api/v1/incidents", a.incidents)
}

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	projects, err := a.db.ListProjectsByUser(r.Context(), user.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	monitors, err := a.svc.List(r.Context(), user)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": monitors})
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := a.svc.Get(r.Context(), user, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *API) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	var in Input
	if !decode(w, r, &in) {
		return
	}
	m, err := a.svc.Create(r.Context(), user, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in Input
	if !decode(w, r, &in) {
		return
	}
	m, err := a.svc.Update(r.Context(), user, id, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.svc.Delete(r.Context(), user, id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) setEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.UserFrom(r.Context())
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		m, err := a.svc.SetEnabled(r.Context(), user, id, enabled)
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}
}

func (a *API) results(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	results, err := a.svc.Results(r.Context(), user, id, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (a *API) incidents(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	rows, err := a.db.ListIncidentsByUser(r.Context(), store.ListIncidentsByUserParams{UserID: user.ID, MaxRows: 100})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": rows})
}

func (a *API) fail(w http.ResponseWriter, err error) {
	var verrs ValidationErrors
	switch {
	case errors.As(err, &verrs):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "dados inválidos", "fields": verrs})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.Is(err, ErrLimitReached):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
	default:
		a.logger.Error("erro na API", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "erro interno"})
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id inválido"})
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON inválido: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
