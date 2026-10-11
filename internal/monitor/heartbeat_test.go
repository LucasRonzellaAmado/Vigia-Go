package monitor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/luquinha/vigia/internal/store"
)

type fakeHB struct {
	monitors map[string]store.Monitor
	touched  map[int64]time.Time
}

func (f *fakeHB) GetMonitorByHeartbeatToken(ctx context.Context, token *string) (store.Monitor, error) {
	m, ok := f.monitors[*token]
	if !ok {
		return store.Monitor{}, pgx.ErrNoRows
	}
	return m, nil
}

func (f *fakeHB) TouchHeartbeat(ctx context.Context, arg store.TouchHeartbeatParams) error {
	f.touched[arg.ID] = *arg.LastHeartbeatAt
	return nil
}

func TestHeartbeat(t *testing.T) {
	const upToken = "aaaaaaaaaaaaaaaaaaaaaaaa"
	const downToken = "bbbbbbbbbbbbbbbbbbbbbbbb"
	f := &fakeHB{
		monitors: map[string]store.Monitor{
			upToken:   {ID: 1, Kind: "heartbeat", Status: "up", Enabled: true},
			downToken: {ID: 2, Kind: "heartbeat", Status: "down", Enabled: true},
		},
		touched: map[int64]time.Time{},
	}
	var triggered []int64
	h := NewHeartbeat(f, func(id int64) { triggered = append(triggered, id) }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	h.Register(mux)

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"GET conhecido", http.MethodGet, "/hb/" + upToken, http.StatusOK},
		{"POST de monitor caído dispara checagem", http.MethodPost, "/hb/" + downToken, http.StatusOK},
		{"HEAD também vale", http.MethodHead, "/hb/" + upToken, http.StatusOK},
		{"token desconhecido", http.MethodGet, "/hb/cccccccccccccccccccccccc", http.StatusNotFound},
		{"token curto", http.MethodGet, "/hb/abc", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
			if w.Code != tt.want {
				t.Fatalf("status = %d, queria %d", w.Code, tt.want)
			}
		})
	}

	if _, ok := f.touched[1]; !ok {
		t.Error("heartbeat do monitor 1 não foi registrado")
	}
	if len(triggered) != 1 || triggered[0] != 2 {
		t.Errorf("deveria disparar checagem só para o monitor caído: %v", triggered)
	}
}
