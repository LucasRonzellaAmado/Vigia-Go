package metrics

import (
	"bufio"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/luquinha/vigia/internal/alert"
	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/store"
)

type Source interface {
	MonitorMetrics(ctx context.Context, since time.Time) ([]store.MonitorMetricsRow, error)
}

type SchedulerStats interface {
	Stats() (tracked, inFlight int)
}

type Collector struct {
	source    Source
	scheduler SchedulerStats
	token     string
	version   string
	started   time.Time
	logger    *slog.Logger

	checksOK     atomic.Int64
	checksFailed atomic.Int64
	alertsSent   atomic.Int64
	alertsFailed atomic.Int64
	checkNanos   atomic.Int64
}

func New(source Source, scheduler SchedulerStats, token, version string, logger *slog.Logger) *Collector {
	return &Collector{source: source, scheduler: scheduler, token: token, version: version, started: time.Now(), logger: logger}
}

func (c *Collector) ObserveCheck(_ store.Monitor, res checker.Result) {
	if res.OK {
		c.checksOK.Add(1)
	} else {
		c.checksFailed.Add(1)
	}
	c.checkNanos.Add(int64(res.Latency))
}

func (c *Collector) ObserveAlert(_ alert.Kind, sent int, err error) {
	c.alertsSent.Add(int64(sent))
	if err != nil {
		c.alertsFailed.Add(1)
	}
}

func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c.token != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(c.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	rows, err := c.source.MonitorMetrics(r.Context(), time.Now().Add(-time.Hour).UTC())
	if err != nil {
		c.logger.Error("coletar métricas", "error", err)
		http.Error(w, "erro coletando métricas", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	bw := bufio.NewWriter(w)
	c.write(bw, rows)
	bw.Flush()
}

func (c *Collector) write(w io.Writer, rows []store.MonitorMetricsRow) {
	e := expo{w: w}

	e.header("vigia_build_info", "gauge", "Versão do Vigia em execução.")
	e.sample("vigia_build_info", labels{"version", c.version, "go_version", runtime.Version()}, 1)

	e.header("vigia_uptime_seconds", "gauge", "Segundos desde que o processo subiu.")
	e.sample("vigia_uptime_seconds", nil, time.Since(c.started).Seconds())

	e.header("vigia_checks_total", "counter", "Checagens executadas desde o start, por resultado.")
	e.sample("vigia_checks_total", labels{"result", "ok"}, float64(c.checksOK.Load()))
	e.sample("vigia_checks_total", labels{"result", "failed"}, float64(c.checksFailed.Load()))

	e.header("vigia_check_duration_seconds_sum", "counter", "Soma das latências das checagens.")
	e.sample("vigia_check_duration_seconds_sum", nil, time.Duration(c.checkNanos.Load()).Seconds())

	e.header("vigia_alerts_sent_total", "counter", "Mensagens de alerta entregues.")
	e.sample("vigia_alerts_sent_total", nil, float64(c.alertsSent.Load()))
	e.header("vigia_alert_failures_total", "counter", "Disparos de alerta com pelo menos um canal falhando.")
	e.sample("vigia_alert_failures_total", nil, float64(c.alertsFailed.Load()))

	if c.scheduler != nil {
		tracked, inFlight := c.scheduler.Stats()
		e.header("vigia_scheduler_monitors", "gauge", "Monitores ativos no scheduler.")
		e.sample("vigia_scheduler_monitors", nil, float64(tracked))
		e.header("vigia_scheduler_in_flight", "gauge", "Checagens rodando agora.")
		e.sample("vigia_scheduler_in_flight", nil, float64(inFlight))
	}

	e.header("vigia_monitor_up", "gauge", "1 no ar, 0 fora, -1 sem dados.")
	for _, r := range rows {
		v := -1.0
		switch r.Status {
		case "up":
			v = 1
		case "down":
			v = 0
		}
		e.sample("vigia_monitor_up", monitorLabels(r), v)
	}

	e.header("vigia_monitor_latency_ms", "gauge", "Latência das checagens bem-sucedidas na última hora.")
	for _, r := range rows {
		if r.Checks == 0 {
			continue
		}
		e.sample("vigia_monitor_latency_ms", append(monitorLabels(r), "quantile", "0.5"), r.P50)
		e.sample("vigia_monitor_latency_ms", append(monitorLabels(r), "quantile", "0.95"), r.P95)
	}

	e.header("vigia_monitor_checks_1h", "gauge", "Checagens na última hora.")
	for _, r := range rows {
		e.sample("vigia_monitor_checks_1h", monitorLabels(r), float64(r.Checks))
	}
	e.header("vigia_monitor_failures_1h", "gauge", "Checagens com falha na última hora.")
	for _, r := range rows {
		e.sample("vigia_monitor_failures_1h", monitorLabels(r), float64(r.Failures))
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	e.header("go_goroutines", "gauge", "Goroutines em execução.")
	e.sample("go_goroutines", nil, float64(runtime.NumGoroutine()))
	e.header("go_memstats_heap_alloc_bytes", "gauge", "Bytes alocados no heap.")
	e.sample("go_memstats_heap_alloc_bytes", nil, float64(ms.HeapAlloc))
	e.header("go_memstats_sys_bytes", "gauge", "Bytes obtidos do sistema operacional.")
	e.sample("go_memstats_sys_bytes", nil, float64(ms.Sys))
	e.header("go_gc_cycles_total", "counter", "Ciclos de GC completos.")
	e.sample("go_gc_cycles_total", nil, float64(ms.NumGC))
	if rss, ok := residentMemory(); ok {
		e.header("process_resident_memory_bytes", "gauge", "Memória residente (RSS) do processo.")
		e.sample("process_resident_memory_bytes", nil, float64(rss))
	}
}

type labels []string

func monitorLabels(r store.MonitorMetricsRow) labels {
	return labels{"monitor_id", strconv.FormatInt(r.ID, 10), "monitor", r.Name, "kind", r.Kind, "project", r.ProjectSlug}
}

type expo struct{ w io.Writer }

func (e expo) header(name, typ, help string) {
	fmt.Fprintf(e.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (e expo) sample(name string, l labels, v float64) {
	io.WriteString(e.w, name)
	if len(l) > 0 {
		io.WriteString(e.w, "{")
		for i := 0; i+1 < len(l); i += 2 {
			if i > 0 {
				io.WriteString(e.w, ",")
			}
			fmt.Fprintf(e.w, `%s="%s"`, l[i], escapeLabel(l[i+1]))
		}
		io.WriteString(e.w, "}")
	}
	fmt.Fprintf(e.w, " %s\n", strconv.FormatFloat(v, 'g', -1, 64))
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func residentMemory() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}
