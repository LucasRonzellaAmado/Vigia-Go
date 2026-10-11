package monitor

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/store"
)

type HeartbeatStore interface {
	GetMonitorByHeartbeatToken(ctx context.Context, token *string) (store.Monitor, error)
	TouchHeartbeat(ctx context.Context, arg store.TouchHeartbeatParams) error
}

type Heartbeat struct {
	store   HeartbeatStore
	trigger func(monitorID int64)
	logger  *slog.Logger
	now     func() time.Time
}

func NewHeartbeat(s HeartbeatStore, trigger func(int64), logger *slog.Logger) *Heartbeat {
	return &Heartbeat{store: s, trigger: trigger, logger: logger, now: time.Now}
}

func (h *Heartbeat) Register(mux *http.ServeMux) {
	mux.Handle("GET /hb/{token}", h)
	mux.Handle("POST /hb/{token}", h)
	mux.Handle("HEAD /hb/{token}", h)
}

func (h *Heartbeat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if len(token) < 16 || len(token) > 64 {
		http.Error(w, "token inválido", http.StatusNotFound)
		return
	}
	m, err := h.store.GetMonitorByHeartbeatToken(r.Context(), &token)
	if err != nil || m.Kind != string(checker.KindHeartbeat) {
		if err != nil && !store.IsNotFound(err) {
			h.logger.Error("heartbeat", "error", err)
			http.Error(w, "erro interno", http.StatusInternalServerError)
			return
		}
		http.Error(w, "token desconhecido", http.StatusNotFound)
		return
	}

	now := h.now().UTC()
	if err := h.store.TouchHeartbeat(r.Context(), store.TouchHeartbeatParams{ID: m.ID, LastHeartbeatAt: &now}); err != nil {
		h.logger.Error("registrar heartbeat", "monitor_id", m.ID, "error", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}
	if m.Enabled && m.Status != "up" && h.trigger != nil {
		h.trigger(m.ID)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write([]byte("OK\n"))
	}
}
