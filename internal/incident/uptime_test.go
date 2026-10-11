package incident

import (
	"math"
	"testing"
	"time"
)

func TestDowntime(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return base.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	from, to := at(0, 0), at(24, 0)

	tests := []struct {
		name      string
		intervals []Interval
		want      time.Duration
	}{
		{"sem incidentes", nil, 0},
		{"um incidente fechado", []Interval{{at(1, 0), at(1, 30)}}, 30 * time.Minute},
		{"incidente em aberto conta até o fim da janela", []Interval{{at(23, 0), time.Time{}}}, time.Hour},
		{"incidente que começou antes da janela é recortado", []Interval{{at(-2, 0), at(1, 0)}}, time.Hour},
		{"incidente que termina depois da janela é recortado", []Interval{{at(23, 30), at(26, 0)}}, 30 * time.Minute},
		{"fora da janela não conta", []Interval{{at(-5, 0), at(-4, 0)}, {at(25, 0), at(26, 0)}}, 0},
		{"sobrepostos não contam em dobro", []Interval{{at(2, 0), at(3, 0)}, {at(2, 30), at(3, 30)}}, 90 * time.Minute},
		{"encostados se somam", []Interval{{at(4, 0), at(5, 0)}, {at(5, 0), at(6, 0)}}, 2 * time.Hour},
		{"fora de ordem", []Interval{{at(10, 0), at(11, 0)}, {at(1, 0), at(2, 0)}}, 2 * time.Hour},
		{"contido dentro de outro", []Interval{{at(1, 0), at(5, 0)}, {at(2, 0), at(3, 0)}}, 4 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Downtime(tt.intervals, from, to); got != tt.want {
				t.Errorf("Downtime = %v, queria %v", got, tt.want)
			}
		})
	}
}

func TestUptime(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	tests := []struct {
		name      string
		intervals []Interval
		from, to  time.Time
		created   time.Time
		want      float64
		wantOK    bool
	}{
		{"100%", nil, base, base.Add(day), base.Add(-day), 1, true},
		{"1h fora em 24h", []Interval{{base, base.Add(time.Hour)}}, base, base.Add(day), base.Add(-day), 1 - 1.0/24, true},
		{"monitor criado no meio da janela usa só o período existente", []Interval{{base.Add(18 * time.Hour), base.Add(21 * time.Hour)}}, base, base.Add(day), base.Add(12 * time.Hour), 0.75, true},
		{"criado depois da janela: sem dados", nil, base, base.Add(day), base.Add(2 * day), 0, false},
		{"tudo fora", []Interval{{base.Add(-time.Hour), time.Time{}}}, base, base.Add(day), base.Add(-day), 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Uptime(tt.intervals, tt.from, tt.to, tt.created)
			if ok != tt.wantOK || math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("Uptime = %v, %v; queria %v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestDailyBars(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, loc)
	created := time.Date(2026, 9, 30, 12, 0, 0, 0, loc)
	intervals := []Interval{
		{time.Date(2026, 10, 1, 23, 0, 0, 0, loc), time.Date(2026, 10, 2, 1, 0, 0, 0, loc)},
	}

	bars := DailyBars(intervals, created, now, 5, loc)
	if len(bars) != 5 {
		t.Fatalf("len = %d", len(bars))
	}

	want := []struct {
		date    string
		hasData bool
		down    time.Duration
		level   string
	}{
		{"2026-09-28", false, 0, "nodata"},
		{"2026-09-29", false, 0, "nodata"},
		{"2026-09-30", true, 0, "up"},
		{"2026-10-01", true, time.Hour, "down"},
		{"2026-10-02", true, time.Hour, "down"},
	}
	for i, w := range want {
		b := bars[i]
		if got := b.Date.Format("2006-01-02"); got != w.date {
			t.Errorf("barra %d: data = %s, queria %s", i, got, w.date)
		}
		if b.HasData != w.hasData || b.Downtime != w.down || b.Level() != w.level {
			t.Errorf("barra %d (%s): %+v level=%s", i, w.date, b, b.Level())
		}
	}
	if bars[3].Incidents != 1 || bars[4].Incidents != 1 {
		t.Errorf("incidente que cruza a meia-noite deve aparecer nos dois dias: %d, %d", bars[3].Incidents, bars[4].Incidents)
	}
	if got := bars[4].Uptime; math.Abs(got-(1-1.0/15)) > 1e-9 {
		t.Errorf("uptime de hoje (15h decorridas, 1h fora) = %v", got)
	}
}
