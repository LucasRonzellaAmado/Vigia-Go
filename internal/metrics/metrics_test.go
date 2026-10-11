package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/store"
)

type fakeSource []store.MonitorMetricsRow

func (f fakeSource) MonitorMetrics(ctx context.Context, since time.Time) ([]store.MonitorMetricsRow, error) {
	return f, nil
}

func TestExposition(t *testing.T) {
	rows := fakeSource{
		{ID: 1, Name: `API "principal"`, Kind: "http", Status: "up", ProjectSlug: "loja", Checks: 60, Failures: 1, P50: 120, P95: 340.5},
		{ID: 2, Name: "Banco", Kind: "tcp", Status: "down", ProjectSlug: "loja", Checks: 0},
	}
	c := New(rows, nil, "", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.ObserveCheck(store.Monitor{}, checker.Result{OK: true, Latency: time.Second})
	c.ObserveCheck(store.Monitor{}, checker.Result{OK: false})

	w := httptest.NewRecorder()
	c.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()

	for _, want := range []string{
		`vigia_monitor_up{monitor_id="1",monitor="API \"principal\"",kind="http",project="loja"} 1`,
		`vigia_monitor_up{monitor_id="2",monitor="Banco",kind="tcp",project="loja"} 0`,
		`vigia_monitor_latency_ms{monitor_id="1",monitor="API \"principal\"",kind="http",project="loja",quantile="0.95"} 340.5`,
		`vigia_checks_total{result="ok"} 1`,
		`vigia_checks_total{result="failed"} 1`,
		"# TYPE vigia_checks_total counter",
		"go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("faltou %q na saída:\n%s", want, body)
		}
	}
	if strings.Contains(body, `monitor_id="2",monitor="Banco",kind="tcp",project="loja",quantile`) {
		t.Error("monitor sem checagens não deveria exportar latência")
	}
}

func TestMetricsToken(t *testing.T) {
	c := New(fakeSource{}, nil, "segredo", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))

	tests := []struct {
		header string
		want   int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer errado", http.StatusUnauthorized},
		{"Bearer segredo", http.StatusOK},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if tt.header != "" {
			r.Header.Set("Authorization", tt.header)
		}
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Errorf("Authorization %q: status %d, queria %d", tt.header, w.Code, tt.want)
		}
	}
}
