package incident

import (
	"slices"
	"time"

	"github.com/luquinha/vigia/internal/store"
)

type Interval struct {
	Start time.Time
	End   time.Time
}

func IntervalsOf(incidents []store.Incident) []Interval {
	out := make([]Interval, 0, len(incidents))
	for _, inc := range incidents {
		iv := Interval{Start: inc.StartedAt}
		if inc.ResolvedAt != nil {
			iv.End = *inc.ResolvedAt
		}
		out = append(out, iv)
	}
	return out
}

func Downtime(intervals []Interval, from, to time.Time) time.Duration {
	if !to.After(from) {
		return 0
	}

	clipped := make([]Interval, 0, len(intervals))
	for _, iv := range intervals {
		end := iv.End
		if end.IsZero() || end.After(to) {
			end = to
		}
		start := iv.Start
		if start.Before(from) {
			start = from
		}
		if end.After(start) {
			clipped = append(clipped, Interval{Start: start, End: end})
		}
	}
	slices.SortFunc(clipped, func(a, b Interval) int { return a.Start.Compare(b.Start) })

	var total time.Duration
	var cur Interval
	for i, iv := range clipped {
		if i == 0 {
			cur = iv
			continue
		}
		if !iv.Start.After(cur.End) {
			if iv.End.After(cur.End) {
				cur.End = iv.End
			}
			continue
		}
		total += cur.End.Sub(cur.Start)
		cur = iv
	}
	if len(clipped) > 0 {
		total += cur.End.Sub(cur.Start)
	}
	return total
}

func Uptime(intervals []Interval, from, to, createdAt time.Time) (float64, bool) {
	if createdAt.After(from) {
		from = createdAt
	}
	if !to.After(from) {
		return 0, false
	}
	window := to.Sub(from)
	down := Downtime(intervals, from, to)
	return 1 - float64(down)/float64(window), true
}

type Day struct {
	Date      time.Time
	Uptime    float64
	Downtime  time.Duration
	Incidents int
	HasData   bool
}

func (d Day) Level() string {
	switch {
	case !d.HasData:
		return "nodata"
	case d.Downtime == 0:
		return "up"
	case d.Uptime >= 0.99:
		return "degraded"
	default:
		return "down"
	}
}

func DailyBars(intervals []Interval, createdAt, now time.Time, days int, loc *time.Location) []Day {
	localNow := now.In(loc)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)

	out := make([]Day, 0, days)
	for i := days - 1; i >= 0; i-- {
		dayStart := today.AddDate(0, 0, -i)
		dayEnd := dayStart.AddDate(0, 0, 1)
		if dayEnd.After(now) {
			dayEnd = now
		}

		d := Day{Date: dayStart}
		if up, ok := Uptime(intervals, dayStart, dayEnd, createdAt); ok {
			from := dayStart
			if createdAt.After(from) {
				from = createdAt
			}
			d.HasData = true
			d.Uptime = up
			d.Downtime = Downtime(intervals, from, dayEnd)
			for _, iv := range intervals {
				end := iv.End
				if end.IsZero() {
					end = now
				}
				if iv.Start.Before(dayEnd) && end.After(dayStart) {
					d.Incidents++
				}
			}
		}
		out = append(out, d)
	}
	return out
}
